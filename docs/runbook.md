# 운영 런북

수집 서버(`transmuxd`)와 재생 서버(`playbackd`)를 운영하면서 알람을 받았을 때 따르는
절차다. 소제목은 알람 이름이나 증상으로 정했으므로 알람 이름으로 검색해 해당 절로
바로 이동한다. 각 절은 증상, 확인, 판단 기준, 조치, 복구 확인, 에스컬레이션 순서로
쓴다.

이 문서는 절차만 다룬다. 다른 내용은 다음 문서를 따른다.

- 엔드포인트, 채널 상태 코드, 메트릭 설계 이유: [README의 수집 서버 모니터링](../README.md#수집-서버-모니터링),
  [알람 설계](../README.md#알람-설계)
- 재생 API, 인덱스와 보존 동작: [솔루션 가이드](solution.md)
- 네트워크 노출 범위와 IAM 권한: [배포 보안 가이드](deployment-security.md)
- 채널 수, 메모리, tmpfs, S3 요청 비용: [용량 모델](capacity-model.md)

여기 인용한 메트릭 이름, 로그 메시지, 설정 키와 기본값은 `internal/`과 `cmd/`의
코드를 그대로 옮긴 것이다. 코드를 바꿀 때는 이 문서도 함께 고친다.

## 시작하기 전에

### 접속 변수와 도구

명령은 `curl`과 `jq`를 사용한다. 수집 서버의 모니터링 API는 인증이 없으므로 내부
네트워크에서만 접근한다([배포 보안 가이드](deployment-security.md#transmuxd의-모니터링-api는-인증이-없다)).

```sh
# 수집 서버 한 대의 모니터링 리스너(http_listen). 샤드마다 바꿔 실행한다.
TRANSMUX=http://127.0.0.1:8080
# 재생 서버. 운영에서는 HTTPS 프록시의 public_url을 쓴다.
PLAYBACK=http://127.0.0.1:8090
```

`GET /v1/admin/status`와 `GET /v1/admin/retention`은 전체 범위(`center_id: "*"`,
`camera_ids: ["*"]`)의 `manage` 권한이 있는 토큰이 필요하다. 아래 명령은 비밀번호를
환경변수에서 읽어 argv와 셸 기록에 남기지 않는다. 계정 이름은 운영 계정으로 바꾼다.

```sh
TOKEN=$(jq -n '{username: "admin", password: env.TRANSMUX_ADMIN_PASSWORD}' |
  curl -fsS -H 'Content-Type: application/json' --data @- "$PLAYBACK/v1/login" |
  jq -r .access_token)
curl -fsS -H "Authorization: Bearer $TOKEN" "$PLAYBACK/v1/admin/status" | jq
```

로그와 설정을 이슈나 채팅에 붙일 때는 카메라 식별자, 주소, 토큰을 가린다.
`/channels`의 `source_url`과 로그의 `source`는 userinfo와 쿼리스트링이 제거된 값이지만
카메라 주소 자체는 남는다.

### 판정에 쓰는 설정 기본값

아래 시간 계산은 이 값들로 한다. 코드 기본값은 `internal/config/config.go`의
`Default()`이고, 참조 솔루션 설정은 `configs/solution-ingest.json`이다. 실제 배포의
값으로 다시 계산한다.

| 키 | 코드 기본값 | 참조 솔루션 설정 |
|---|---|---|
| `segment.target_duration` | `5s` | `4s` |
| `segment.max_gop_slack` | `10s` | `10s` |
| `segment.local_list_size` | `10` | `10` |
| `upload.put_timeout` | `15s` | 기본값 |
| `upload.max_attempts` | `3` | 기본값 |
| `upload.max_concurrent` | `64` | `16` |
| `ffmpeg.stall_timeout` | `30s` | `20s` |
| `ffmpeg.startup_timeout` | `20s` | `20s` |
| `ffmpeg.shutdown_grace` | `5s` | 기본값 |
| `reconnect.base` / `reconnect.max` / `reconnect.factor` | `1s` / `30s` / `2.0` | 기본값 |
| `reconnect.reset_after` | `60s` | 기본값 |
| `lease.ttl` | `45s` | `20s` |
| `lease.renew_interval` | `10s` | `5s` |
| `lease.max_clock_skew` | `2s` | `2s` |
| `lease.operation_timeout` | `5s` | `3s` |
| `cameras.poll_interval` | `30s` | `3s` |

### 정상 채널의 정의

`/healthz`, `/readyz`, `/channels`의 `healthy`는 다음 세 조건을 모두 만족할 때 참이다
(`internal/channel/worker.go`의 `healthyLocked`).

1. 상태가 `receiving`이다.
2. 마지막 세그먼트 저장이 **gap 허용치** 안에 있다.
3. 마지막 매니페스트 저장이 gap 허용치 안에 있다.

gap 허용치는 `segment.target_duration + segment.max_gop_slack + upload.put_timeout`이다.
코드 기본값으로 30초, 참조 솔루션 설정으로 29초다. 아래 PromQL 예시의 `30`은 이 값이다.

`/readyz`는 다음 경우 503을 돌려준다. `reason` 필드에 이유가 들어간다.

| `reason` | 뜻 |
|---|---|
| `camera roster not loaded yet` | 카메라 목록을 한 번도 불러오지 못했다 |
| `no channels assigned` | 이 샤드에 배정된 채널이 없다 |
| `majority of channels degraded` | 정상이 아닌 채널이 전체의 50%를 넘는다 |

`/healthz`는 항상 200이다. `status`가 `starting`, `ok`, `degraded`, `down` 중 하나이고
`channels_total`, `channels_healthy`, `channels_degraded`를 함께 준다. Zabbix나 WhaTap은
상태 코드가 아니라 이 필드로 알람한다. `/livez`는 카메라와 S3 상태를 반영하지 않으므로
알람에 쓰지 않는다.

### 메트릭 라벨

- 채널 메트릭(`transmux_channel_*`)의 라벨은 `center_id`, `camera_id`뿐이다.
  샤드별로 묶을 때는 Prometheus가 스크랩 대상마다 붙이는 `instance` 라벨을 쓴다.
- `transmux_channels_running`, `transmux_roster_*`, `transmux_build_info`에는
  `shard_id` 라벨이 있다.
- `transmux_object_put_*`에는 라벨이 없다. 프로세스 전체의 합계다.
- 카메라가 목록에서 빠지거나 수집이 중지되면 그 채널의 시계열은 삭제된다.
  시계열이 사라진 것을 장애로 해석하지 않는다.
- `playbackd`는 `/metrics`를 제공하지 않는다([#29](https://github.com/jeonghun-app/transmux/issues/29)).

## 알람 기준

알람 이름은 예시다. 조건의 메트릭 이름은 모두 `transmuxd`가 실제로 내보내는 것이다.
임계값은 시작점이므로 운영 데이터를 보고 조정한다.

| 알람 | 조건 요약 | 심각도 | 대응 절 |
|---|---|---|---|
| `TransmuxChannelsDown` | 수신 중이 아닌 채널 비율이 20% 초과, 5분 지속 | 높음 | [채널 다운 비율 상승](#채널-다운-비율-상승) |
| `TransmuxCameraUnowned` | 어느 샤드도 발행하지 않는 카메라, 5분 지속 | 높음 | [채널 다운 비율 상승](#채널-다운-비율-상승) |
| `TransmuxChannelFailed` | `failed` 상태 채널 존재 | 높음 | [failed 채널 복구](#failed-채널-복구) |
| `TransmuxUploadFailing` | 재시도 후에도 실패한 업로드 발생 | 높음 | [업로드 실패와 지연](#업로드-실패와-지연) |
| `TransmuxUploadBacklog` | 스풀 정체나 매니페스트 지연 | 중간 | [업로드 실패와 지연](#업로드-실패와-지연) |
| `TransmuxSegmentGap` | 수신 중 채널의 세그먼트 간격 초과 | 중간 | [세그먼트 누락과 정체](#세그먼트-누락과-정체) |
| `TransmuxSegmentsLost` | 업로드 전에 회수된 세그먼트 발생 | 중간 | [세그먼트 누락과 정체](#세그먼트-누락과-정체) |
| `TransmuxLeaseLost` | 소유권 상실이나 갱신 실패 | 높음 | [lease 소유권 상실](#lease-소유권-상실) |
| `TransmuxDualWriter` | 매니페스트·세그먼트 충돌 발생 | 긴급 | [lease 소유권 상실](#lease-소유권-상실) |
| `PlaybackUnavailable` | 재생 서버 `/readyz` 실패나 5xx 증가 | 높음 | [재생 서버 오류율 상승](#재생-서버-오류율-상승) |

### 채널 다운 비율 상승

**증상**: 여러 카메라의 라이브가 끊기거나 `/healthz`의 `status`가 `degraded` 또는
`down`이다.

**알람 조건**

```promql
# TransmuxChannelsDown: 샤드별 수신 중(2)이 아닌 채널 비율. for: 5m
  sum by (instance) (transmux_channel_state != bool 2)
/
  count by (instance) (transmux_channel_state)
> 0.2

# TransmuxCameraUnowned: 어느 샤드도 lease와 fence를 모두 가지지 못한 카메라. for: 5m
max by (center_id, camera_id) (transmux_channel_lease_held) == 0
```

Zabbix나 WhaTap에서는 `/healthz`의 `channels_degraded / channels_total`로 같은 기준을 건다.
이 값은 위 PromQL보다 엄격하다. 세그먼트와 매니페스트 시각까지 확인한다.

같은 `cameras.shard_filter`로 대기 복제본을 둔 경우, 대기 복제본의 채널은 모두
`waiting_ownership`(8)이므로 `TransmuxChannelsDown`이 항상 울린다. 대기 복제본은 이 알람에서
제외하고 카메라 단위의 `TransmuxCameraUnowned`로 감시한다. 모든 샤드가 내려가면
시계열 자체가 사라지므로 Prometheus의 `up == 0` 알람을 함께 둔다.

**확인**

```sh
curl -fsS "$TRANSMUX/healthz" | jq '{status, channels_total, channels_healthy, channels_degraded}'
curl -fsS "$TRANSMUX/channels" | jq -r '.channels[] | select(.healthy | not)
  | [.center_id, .camera_id, .state, .reconnects, .upload_failures, (.last_error // "")] | @tsv'
```

**판단 기준**: 정상이 아닌 채널의 `state`를 센다.

| 대부분의 `state` | 판단 | 다음 절 |
|---|---|---|
| `receiving`인데 `healthy: false` | 카메라는 정상이고 저장이 안 된다 | [업로드 실패와 지연](#업로드-실패와-지연) |
| `disconnected`, `reconnecting`, `starting` | 카메라나 RTSP 경로 문제다 | [ffmpeg이 반복 재시작되는 카메라](#ffmpeg이-반복-재시작되는-카메라) |
| `waiting_ownership` | 다른 샤드가 소유 중이거나 S3를 읽지 못한다 | [lease 소유권 상실](#lease-소유권-상실) |
| `failed` | 자동 복구되지 않는다 | [failed 채널 복구](#failed-채널-복구) |

한 샤드의 거의 모든 채널이 동시에 나빠졌으면 카메라보다 S3나 샤드 자체를 먼저 의심한다.
한 센터의 카메라만 나빠졌으면 그 센터의 카메라 네트워크를 먼저 확인한다.

카메라 목록 문제는 채널 상태에 나타나지 않으므로 따로 본다.

- 목록이 `max_channels`를 넘으면 초과분은 시작되지 않고 시계열도 없다.
  `increase(transmux_roster_rejected_total[10m]) > 0`과 로그
  `roster exceeds shard capacity, ignoring the excess`로 잡는다.
- 목록을 불러오지 못하면 기존 채널을 그대로 유지한다.
  `increase(transmux_roster_reconcile_failures_total[10m]) > 0`과 로그
  `camera roster load failed, keeping current channels`로 잡는다. `/healthz`의
  `shard.last_reconcile_error`에 마지막 오류가 있다.

**1차 조치**: 원인 절차로 이동한다. 카메라 몇 대 때문에 샤드를 재시작하지 않는다.
같은 샤드의 정상 채널까지 끊긴다.

**복구 확인**: `/healthz`의 `channels_degraded`가 0으로 돌아오고 `TransmuxCameraUnowned`가
해소된다.

**에스컬레이션**: 15분 안에 원인 절을 특정하지 못하면 당직 책임자에게 넘긴다.

### 업로드 실패와 지연

**증상**: 채널이 `receiving`인데 `healthy: false`이거나, 라이브가 몇십 초 늦거나,
최근 녹화가 비어 있다.

**알람 조건**

```promql
# TransmuxUploadFailing: 재시도까지 모두 실패한 세그먼트·매니페스트 업로드
sum by (instance) (increase(transmux_channel_segment_upload_failures_total[5m])) > 0

sum by (instance) (increase(transmux_channel_manifest_upload_failures_total[5m])) > 0

# 저장소 PUT 실패율(재시도 후 실패 / 전체). for: 5m
  sum by (instance) (rate(transmux_object_put_failures_total[5m]))
/
  (sum by (instance) (rate(transmux_object_put_total[5m]))
   + sum by (instance) (rate(transmux_object_put_failures_total[5m])))
> 0.01

# TransmuxUploadBacklog: ffmpeg은 세그먼트를 만드는데 저장이 30초 넘게 밀린다. for: 2m
(transmux_channel_last_ffmpeg_segment_timestamp_seconds
  - transmux_channel_last_segment_timestamp_seconds) > 30

# 스풀의 가장 오래된 세그먼트가 회수 시점(local_list_size x 세그먼트 길이)의 절반을 넘었다.
# 10은 segment.local_list_size 값으로 바꾼다.
transmux_channel_spool_oldest_seconds
  > 0.5 * 10 * transmux_channel_last_segment_duration_seconds

# 세그먼트는 저장되지만 매니페스트가 30초 넘게 갱신되지 않는다(재생에 안 보인다). for: 1m
transmux_channel_seconds_since_manifest > 30 and transmux_channel_state == 2
```

**확인**

```sh
curl -fsS "$TRANSMUX/metrics" | grep -E '^transmux_object_put_(total|failures_total|retries_total|in_flight) '
curl -fsS "$TRANSMUX/channels" | jq -r '.channels[] | select(.state == "receiving" and (.healthy | not))
  | [.center_id, .camera_id, .upload_failures, (.last_error // "")] | @tsv'
```

로그에서는 `drain error`, `final drain incomplete`, `carry-over drain incomplete, spooled segments will be discarded`를 찾는다.
S3가 HTTP 오류로 답하면 `s3 put <key>: http 503 (request-id ...)`처럼 상태 코드와 요청 ID를
남긴다. 응답이 없었던 연결 오류에는 상태 코드가 없다.

**판단 기준**

- `last_error`나 로그의 상태 코드가 `http 403`이면 권한이나 자격증명 문제다.
  [배포 보안 가이드](deployment-security.md#aws-자격증명)의 수집 역할 권한을 확인한다.
- `http 5xx`, 타임아웃, 연결 오류가 여러 샤드에서 동시에 나면 저장소 장애다.
  [스토리지(S3) 장애](#스토리지s3-장애)로 이동한다.
- 한 샤드에서만 나고 `transmux_object_put_in_flight`가 `upload.max_concurrent` 근처에
  머물러 있으면 그 샤드의 네트워크나 업로드 동시성이 부족하다.
  `transmux_object_put_retries_total` 증가가 함께 보이면 S3 쪽 지연이다.
- 매니페스트 실패만 있고 세그먼트 실패가 없으면 매니페스트 키의 권한이나 Lifecycle 규칙을
  확인한다. Lifecycle은 라이브 매니페스트와 `_transmux/`를 제외해야 한다.

**1차 조치**

- 업로드 실패만으로 샤드를 재시작하지 않는다. 재시작하면 tmpfs 스풀이 사라져 아직 올리지
  못한 세그먼트가 유실된다. ffmpeg은 저장소 장애 때 일부러 재시작하지 않는다.
- 권한 문제면 IAM 정책을 고친다. 프로세스 재시작 없이 다음 시도부터 반영된다.
- 동시성 부족이면 `upload.max_concurrent`를 늘리거나 샤드당 채널 수를 줄인다.
  설정 변경은 재시작이 필요하므로 [샤드 재시작·교체](#샤드-재시작교체) 절차를 따른다.

**복구 확인**: 두 타임스탬프의 차이가 세그먼트 길이 안으로 돌아오고
`transmux_channel_seconds_since_manifest`가 gap 허용치 아래로 내려간다.

**에스컬레이션**: 저장소 장애면 클라우드 지원 경로로, 권한 문제면 IAM 담당자에게 넘긴다.

### 세그먼트 누락과 정체

**증상**: 특정 카메라의 녹화에 구멍이 있거나 라이브가 멈춘다.

**알람 조건**

```promql
# TransmuxSegmentGap: 수신 중 채널이 gap 허용치보다 오래 세그먼트를 저장하지 못했다. for: 1m
transmux_channel_segment_gap_alarm == 1

# TransmuxSegmentsLost: 업로드 전에 ffmpeg이 회수했거나 재연결 때 폐기한 세그먼트
increase(transmux_channel_segments_lost_total[15m]) > 0

# 카메라가 PDT를 주지 않아 날짜 디렉터리를 업로드 시각으로 대신했다
increase(transmux_channel_missing_pdt_total[1h]) > 0
```

`transmux_channel_segment_gap_alarm`은 `receiving` 상태에서만 1이 된다. 재연결 중인
카메라는 [ffmpeg이 반복 재시작되는 카메라](#ffmpeg이-반복-재시작되는-카메라)의 알람으로 잡는다.

**확인**

```sh
CENTER=center-01 CAMERA=cam-01
curl -fsS "$TRANSMUX/channels/$CENTER/$CAMERA" | jq
curl -fsS "$TRANSMUX/metrics" | grep -E "camera_id=\"$CAMERA\"" | grep -E "center_id=\"$CENTER\""
```

로그에서 `segments reclaimed before upload`, `discarding spooled segments that were never stored`,
`segment has no EXT-X-PROGRAM-DATE-TIME, filing it under the current time`를 찾는다.

**판단 기준**

- `transmux_channel_last_segment_duration_seconds`가 gap 허용치(기본 30초)에 가까우면 카메라
  GOP가 길다. 정상 카메라를 gap 알람이 잡는 경우다. 세그먼트 길이는 목표 길이가 아니라
  카메라의 I-프레임 간격이 정한다.
- `segments reclaimed before upload`는 업로드가 `local_list_size × 세그먼트 길이`보다 오래
  밀렸다는 뜻이다. [업로드 실패와 지연](#업로드-실패와-지연)과 같은 원인이다.
- `discarding spooled segments that were never stored`는 ffmpeg 재시작 직전에 남은 세그먼트를
  지웠다는 뜻이다. 재연결이 원인이다.
- 녹화 구간만 비어 보이고 S3에는 객체가 있으면 수집 문제가 아니다.
  [녹화 인덱스 불일치와 재구축](#녹화-인덱스-불일치와-재구축)으로 이동한다.

**1차 조치**

- 긴 GOP 카메라면 카메라의 I-프레임 간격을 줄이거나 `segment.max_gop_slack`을 늘린다.
  `ffmpeg.stall_timeout`과 `ffmpeg.startup_timeout`은 `target_duration + max_gop_slack`보다
  커야 한다. 설정 검증(`-validate`)이 이를 강제한다.
- PDT 누락은 카메라 펌웨어나 RTSP 타임스탬프 문제다. 해당 카메라의 녹화 날짜가 업로드
  시각 기준이 되므로 자정 근처 녹화가 다음 날짜로 들어갈 수 있다는 점을 기록한다.

**복구 확인**: `transmux_channel_segment_gap_alarm`이 0이고 `segments_lost_total`이 더 늘지 않는다.

**에스컬레이션**: 카메라 설정 변경은 카메라 관리 담당자에게 요청한다.

### lease 소유권 상실

**증상**: 채널이 `waiting_ownership`에 머물거나, 소유권을 잃고 `failed`가 됐다.
최악의 경우 두 샤드가 같은 카메라를 발행한다.

**알람 조건**

```promql
# TransmuxLeaseLost
increase(transmux_channel_lease_lost_total[10m]) > 0

increase(transmux_channel_lease_renew_failures_total[5m]) > 0

# 갱신이 한 번 이상 빠졌다. 정상이면 ttl - max_clock_skew - renew_interval 아래로
# 내려가지 않는다. 30은 코드 기본값(33초) 기준이다. 참조 솔루션 설정(13초)이면 10으로 둔다.
transmux_channel_lease_held == 1 and transmux_channel_lease_seconds_remaining < 30

# TransmuxDualWriter: 두 프로세스가 한 카메라를 발행하려 했다. 즉시 조사한다.
increase(transmux_channel_manifest_conflicts_total[10m]) > 0

increase(transmux_channel_segment_conflicts_total[10m]) > 0
```

`transmux_channel_lease_acquire_failures_total`은 S3가 답하지 않거나 fence 경합에 져서
소유권을 확정하지 못한 횟수다. `waiting_ownership`이 길어질 때 이 값이 늘면 S3나 권한
문제다. `transmux_channel_lease_release_failures_total`은 종료 시 release가 실패한 횟수다.
이 값이 늘면 다음 소유자는 강제 종료 때의 인수 시간을 기다린다.

`transmux_channel_lease_contended_total`은 롤링 배포 중에 잠시 늘었다가 멈추는 것이
정상이다. 대기 복제본은 `lease.renew_interval`마다 계속 늘린다. 대기 복제본이 없는데
15분 넘게 계속 늘면 두 샤드의 `cameras.shard_filter` 배정이 겹친 것이다.

**확인**

```sh
curl -fsS "$TRANSMUX/metrics" | grep -E '^transmux_channel_(lease_held|lease_seconds_remaining|state)\{'
curl -fsS "$TRANSMUX/channels" | jq -r '.channels[] | select(.state == "waiting_ownership" or .state == "failed")
  | [.center_id, .camera_id, .state, (.last_error // "")] | @tsv'
```

로그 메시지와 뜻은 다음과 같다.

| 로그 `msg` | 뜻 |
|---|---|
| `camera owned by another shard, waiting` | 다른 프로세스가 살아 있는 lease를 가졌다. `error`는 `camera lease held by another owner: <shard_id> until <만료 시각>`이다 |
| `cannot establish camera ownership, retrying` | S3를 읽지 못해 소유권을 모른다. `lease.renew_interval`마다 재시도한다 |
| `lease renewal failed, will retry` | 갱신이 일시적으로 실패했다 |
| `lease deadline passed without a successful renewal, stopping` | 자기 기한 안에 갱신하지 못해 발행을 멈췄다. 이후 `failed`가 된다 |
| `camera lease lost to another owner, stopping` | 갱신 루프가 발행을 멈췄다. `error`에 `renewal refused, another owner holds`가 있으면 다른 프로세스가 인수한 것이다. `error`가 `camera lease lost`뿐이거나 `completed after the local deadline`이 있으면 자기 lease 기한이 지난 것이다 |
| `channel disabled: another shard owns the published stream` | 발행 경로에서 소유권이 없다고 판정했다. `error`에 `manifest was overwritten by another writer`나 `was written by another owner`가 있으면 다른 기록자를 발견한 것이다. `error`가 `camera lease lost`뿐이거나 `refusing to publish past the lease deadline`이 있으면 자기 lease 기한이 지난 것이다 |
| `channel disabled: the lease object is not usable` | lease 객체가 손상됐거나 다른 형식이다 |
| `manifest changed under us while fencing, re-reading` | fence 직전에 다른 기록자가 매니페스트를 바꿨다 |

**판단 기준**

- `waiting_ownership`이면 `error`의 `shard_id`와 만료 시각을 본다. 다른 샤드면 그 샤드가 아직
  소유 중이다. 롤링 배포 중이면 정상이다. 자기와 같은 `shard_id`면 강제 종료된 이전 프로세스의
  lease다. 만료 시각 + 2 × `max_clock_skew` 뒤에 인수된다. 그 뒤에도 계속되면 이전 프로세스가
  아직 살아 있는지 확인한다.
- `cannot establish camera ownership, retrying`이 반복되면 S3 장애나 권한 문제다.
  `s3:ListBucket`이 없으면 AWS S3가 없는 키에 403을 돌려줘 신규 채널이 이 상태에 머문다
  ([배포 보안 가이드](deployment-security.md#aws-자격증명)).
- 갱신 실패 뒤 `lease deadline passed ...`가 나오거나, `error`가 `camera lease lost`뿐인
  `camera lease lost to another owner, stopping`이 나왔으면 S3 장애가 소유권 기한을 넘긴 것이다.
  [스토리지(S3) 장애](#스토리지s3-장애)를 따른다.
- `TransmuxDualWriter`는 두 프로세스가 같은 카메라를 쓰려 했다는 뜻이다. 원인 후보는
  lease를 모르는 구버전 기록자와, Lifecycle 규칙이나 사람이 활성 lease를 지운 경우다.
  두 샤드가 동시에 인수를 시도한 경합도 매니페스트 충돌을 한 번 올린다. 이때는
  `manifest changed under us while fencing, re-reading` 로그가 함께 남고 더 늘지 않는다.

**1차 조치**

- `TransmuxDualWriter`면 해당 카메라를 쓰는 모든 프로세스를 찾는다. lease를 모르는 구버전
  기록자가 있으면 먼저 정지한다. 신·구 혼재는 lease로 보호되지 않는다.
- Lifecycle 규칙이 `_transmux/`와 라이브 매니페스트를 제외하는지 확인한다. 활성 lease를
  지우면 강제 failover가 일어난다.
- 소유권을 잃은 채널은 스스로 복구되지 않는다. 원인을 없앤 뒤
  [failed 채널 복구](#failed-채널-복구)를 따른다.
- lease 객체(`{prefix}/{center_id}/{camera_id}/_transmux/lease.json`)를 손으로 지우지 않는다.
  예외는 `the lease object is not usable`로 확인된 손상 객체뿐이다. 이 경우에도 그
  카메라를 발행하는 프로세스가 없음을 먼저 확인한다.

**복구 확인**: 카메라마다 정확히 한 샤드에서 `transmux_channel_lease_held` = 1이고,
`transmux_channel_lease_contended_total`이 더 늘지 않는다.

**에스컬레이션**: `TransmuxDualWriter`는 긴급이다. 원인을 확인하지 못했으면 즉시 개발팀에
넘긴다. 영향받은 구간의 매니페스트와 세그먼트 키 목록(자격증명 제외)을 함께 전달한다.

### 재생 서버 오류율 상승

**증상**: 운영자 화면이 열리지 않거나, 라이브·녹화 재생과 내보내기가 실패한다.

`playbackd`는 메트릭을 내보내지 않고 요청 접근 로그도 남기지 않는다([#29](https://github.com/jeonghun-app/transmux/issues/29)).
오류율은 다음 세 가지로 본다.

1. **앞단 HTTPS 프록시나 로드 밸런서의 5xx 비율.** 오류율의 1차 신호다. 카메라가 끊긴
   라이브 요청의 `503 camera_offline`과 동시 재생 한도의 `503 playback_capacity`도 여기에
   포함된다. 이 둘은 `playbackd` 로그에 남지 않는다.
2. **`/readyz` 외부 점검.** 카메라 목록을 읽지 못하면 `503`과 오류 코드
   `catalog_unavailable`을 돌려준다. `/healthz`는 프로세스가 응답하면 항상 200이다.
3. **경고 로그.** 저장소나 인덱스 오류로 `503 temporarily_unavailable`을 돌려줄 때마다
   작업 이름을 `msg`로 한 `WARN` 로그를 남긴다.

```sh
curl -fsS -o /dev/null -w '%{http_code}\n' "$PLAYBACK/readyz"
```

`503 temporarily_unavailable`에 대응하는 로그 `msg` 값은 다음과 같다.
`serve media`, `load live playlist`, `load playback roster`, `issue playback session`,
`store recording session`, `renew playback session`, `sign renewed playback session`,
`query recording index`, `read recording coverage`, `load camera catalog`, `load center catalog`,
`load managed roster`, `load roster for edit`, `save camera roster`, `load retention roster`,
`preview retention`, `create export job`, `plan export`, `check export disk space`,
`issue export download`, `sign export download`, `open export`.

JSON 로그를 파일로 받았다면 다음처럼 분당 건수를 센다.

```sh
jq -r 'select(.level == "WARN" and (.msg | IN("serve media", "load live playlist",
  "issue playback session", "query recording index", "load camera catalog")))
  | "\(.time[0:16]) \(.msg)"' playbackd.log | sort | uniq -c
```

**확인**

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" "$PLAYBACK/v1/admin/status" |
  jq '{index, retention, active_media_requests, active_exports, login}'
```

**판단 기준**

- `/readyz`가 503이고 로그에 `load camera catalog`나 `load playback roster`가 있으면 카메라 목록
  객체를 읽지 못한다. S3 장애나 재생 역할의 권한 문제다. [스토리지(S3) 장애](#스토리지s3-장애)를 본다.
- `serve media`가 대부분이면 미디어 GET이 실패한다. 같은 시각에 수집 서버의 업로드 실패가
  있는지 함께 본다.
- `query recording index`, `store recording session`이 대부분이면 bbolt 인덱스 파일의 디스크
  문제다. 디스크 여유와 I/O 오류를 확인한다.
- `active_media_requests`가 `max_streams`(기본 128)에 붙어 있으면 동시 재생 한도다.
  `503 playback_capacity`가 프록시 5xx를 채운다.
- `login.overflow_refused`가 늘면 클라이언트 추적 표가 가득 찼다
  ([솔루션 가이드](solution.md#인증과-재생)).
- `index.failures_since_start`나 `retention.errors_since_start`가 늘면 로그의
  `recording index scan failed`, `recording retention pass failed`를 본다.

**1차 조치**

- 저장소나 권한 문제면 [스토리지(S3) 장애](#스토리지s3-장애)를 따른다.
- 동시 재생 한도면 `max_streams`를 늘리기 전에 재생 서버의 CPU, 네트워크, S3 GET 비용을
  확인한다. 설정 변경은 재시작이 필요하다.
- 재생 서버를 재시작해도 수집은 계속된다. 다만 진행 중인 내보내기 작업 목록은 메모리에
  있으므로 사라진다.

**복구 확인**: `/readyz`가 200이고 프록시 5xx 비율이 기준선으로 돌아온다.

**에스컬레이션**: 인덱스 파일 손상이 의심되면 [녹화 인덱스 불일치와 재구축](#녹화-인덱스-불일치와-재구축)으로 넘어가기 전에 개발팀과 상의한다.

## 절차

### 샤드 재시작·교체

설정 변경, 이미지 교체, 호스트 교체, 샤드 간 카메라 이동에 쓴다.

#### lease 인수 시간

새 프로세스가 카메라를 인수하는 시점은 이전 프로세스가 어떻게 끝났는지에 따라 다르다.
같은 샤드를 재시작해도 새 프로세스는 새 소유자다. 소유자 식별자는 프로세스마다 새로
만들어지기 때문이다(`internal/channel/lease.go`).

| 이전 프로세스 종료 | 인수 조건 | 최악 인수 시간 | 코드 기본값 | 참조 솔루션 설정 |
|---|---|---|---|---|
| 정상 종료(SIGTERM 후 release 성공) | lease가 `released` tombstone이 된다 | `renew_interval` | 10초 | 5초 |
| 강제 종료, OOM, 정지, 네트워크 단절 | 마지막 갱신 시각 + `ttl` + 2 × `max_clock_skew` 경과 | `ttl + 2 × max_clock_skew + renew_interval` | 59초 | 29초 |

- 대기하는 쪽은 `lease.renew_interval`마다 다시 시도한다. 그래서 두 경우 모두 이 간격만큼
  더 기다릴 수 있다.
- 강제 종료 시간은 마지막으로 성공한 갱신부터 센다. 그 갱신은 종료 시각보다 최대
  `renew_interval` 앞이므로 종료 시각부터 재면 위 값보다 조금 짧다.
- 대기하는 쪽은 상대 시계가 느릴 수 있다고 보고 `max_clock_skew`를 두 번 더한다. 호스트는
  NTP로 시계를 맞춰야 한다.
- 인수 뒤에는 매니페스트 fence 쓰기와 ffmpeg 첫 세그먼트까지 더 걸린다. 첫 세그먼트는
  보통 세그먼트 길이 하나이고 `ffmpeg.startup_timeout`을 넘지 않는다.
- 이전 소유자가 살아 있지만 S3에 닿지 못하면 마지막 갱신 + `ttl − max_clock_skew`에
  스스로 발행을 멈춘다. 새 소유자는 그보다 3 × `max_clock_skew` 뒤에 인수할 수 있다.

참조 솔루션 스택(`make solution-up`)에서 같은 샤드를 재시작해 측정한 값은 다음과 같다.
정상 종료 뒤에는 이전 프로세스 종료 1.4초 뒤에 인수했다. SIGKILL 뒤에는 25초 뒤에 인수했고,
대기 중에는 `error`에 자기와 같은 `shard_id`가 보였다.

#### 종료 유예 시간

SIGTERM을 받으면 채널마다 ffmpeg 종료(`ffmpeg.shutdown_grace`, 기본 5초), 마지막 업로드
(최대 15초), lease release(`lease.operation_timeout`, 기본 5초)를 차례로 한다. 모든 채널은
동시에 종료한다. 오케스트레이터의 종료 유예가 이 합계보다 짧으면 release 전에 SIGKILL을
받는다. 그러면 남은 세그먼트를 잃고 다음 소유자는 강제 종료 시간을 기다린다.

참조 compose(`deploy/docker-compose.solution.yml`)는 `stop_grace_period: 45s`를 쓴다.
ECS `stopTimeout`과 Kubernetes `terminationGracePeriodSeconds`의 기본값 30초는 여유가
적으므로 45초 이상으로 설정한다.

#### 배포 방식 주의

새 태스크의 `/readyz`는 이전 태스크가 lease를 놓을 때까지 503
(`majority of channels degraded`)이다. 새 태스크의 채널이 모두 `waiting_ownership`이기
때문이다. 배포 도구가 새 태스크의 readiness를 기다린 뒤에 이전 태스크를 멈추는 방식이면
두 태스크가 서로를 기다린다.

- 컨테이너 헬스 체크와 liveness는 `/livez`로 한다. 참조 compose와 같다.
- `transmuxd`의 `/readyz`를 배포 진행 조건으로 쓰지 않는다. 또는 이전 태스크를 먼저
  멈추는 방식(Kubernetes `Recreate`, ECS 최소 정상 비율 0%)을 쓴다.

#### 절차

1. 대상 샤드와 버전을 기록한다.

   ```sh
   curl -fsS "$TRANSMUX/metrics" | grep '^transmux_build_info'
   curl -fsS "$TRANSMUX/healthz" | jq '{status, channels_total, channels_healthy}'
   ```

2. 설정을 바꿨으면 새 이미지로 먼저 검증한다. 알 수 없는 키나 잘못된 값이 있으면
   기동하지 않는다.

   ```sh
   docker run --rm -v "$PWD/config.json:/etc/transmux/config.json:ro" \
     ghcr.io/jeonghun-app/transmux:<version> -validate
   ```

3. 오케스트레이터로 교체한다. 이전 태스크에는 SIGTERM을 보내고 위 종료 유예를 준다.
4. 이전 태스크 로그에서 `shutdown requested, draining channels`, `all channels stopped`,
   `transmuxd stopped cleanly`를 확인한다. `lease release failed; it will expire on its own`가
   있으면 그 카메라는 강제 종료 시간만큼 늦게 인수된다.
5. 새 태스크 로그에서 `camera lease acquired`와
   `published stream fenced, this shard is now the writer`를 확인한다.

**복구 확인**

```sh
curl -fsS "$TRANSMUX/healthz" | jq '{status, channels_total, channels_healthy, channels_degraded}'
curl -fsS "$TRANSMUX/metrics" | grep -cE '^transmux_channel_lease_held\{.*\} 1$'
```

`lease_held`가 1인 채널 수가 `channels_total`과 같고 `transmux_channel_lease_contended_total`이
더 늘지 않으면 끝난다.

**에스컬레이션**: 강제 종료 시간의 두 배가 지나도 `waiting_ownership`이 남으면
[lease 소유권 상실](#lease-소유권-상실)을 따른다.

#### 샤드 간 카메라 이동

공유 목록(`cameras.provider: "object"`)에서 카메라의 `shard_id`를 바꾸면 이전 샤드가 다음
목록 갱신(`cameras.poll_interval`)에서 채널을 멈추고 lease를 놓는다. 새 샤드는 자신의 다음
목록 갱신에서 채널을 시작한다. 두 샤드의 `max_channels`와 재생 설정의 `shards` 한도를 먼저
확인한다([솔루션 가이드](solution.md#카메라-관리와-샤딩)).

### 스토리지(S3) 장애

**증상**: 여러 샤드에서 동시에 `TransmuxUploadFailing`과 `TransmuxLeaseLost`가 울린다.
재생 서버의 `/readyz`가 503이 될 수 있다.

**확인**

```sh
curl -fsS "$TRANSMUX/metrics" | grep -E '^transmux_object_put_(failures_total|retries_total|in_flight) '
curl -fsS "$TRANSMUX/channels" | jq -r '[.channels[].state] | group_by(.) | map({(.[0]): length}) | add'
```

S3 서비스 상태 페이지와 버킷의 요청 지표(5xx, 지연)를 함께 본다.

**판단 기준**: 장애 동안 수집 서버는 다음과 같이 움직인다.

- ffmpeg은 계속 돈다. 저장소 장애로는 ffmpeg을 재시작하지 않는다. 세그먼트는 tmpfs 스풀에
  쌓이고 `transmux_channel_spool_oldest_seconds`가 오른다. 채널 상태는 `receiving`이지만
  `healthy`는 거짓이 된다.
- 스풀 세그먼트는 `local_list_size × 세그먼트 길이`가 지나면 ffmpeg이 지운다. 5초 세그먼트면
  약 50초다. 그 뒤에는 `transmux_channel_segments_lost_total`이 오른다.
- lease 갱신이 실패한다(`lease renewal failed, will retry`). 마지막 성공 갱신 +
  `ttl − max_clock_skew` 시점에 채널은 발행을 멈추고 `failed`가 된다. 장애 시작부터
  코드 기본값으로 33∼43초, 참조 솔루션 설정으로 13∼18초 뒤다. 로그에는
  `lease deadline passed without a successful renewal, stopping`이나
  `camera lease lost to another owner, stopping`(`error`는 `camera lease lost`)이 남고,
  이어서 `channel disabled: ...`가 남는다.
- `failed`가 된 채널은 S3가 돌아와도 스스로 복구되지 않는다. 참조 솔루션 스택에서 S3를
  멈추면 17초 뒤 세 채널이 모두 `failed`가 됐고, S3를 되살린 뒤에도 그대로였다.

장애가 대략 `ttl − max_clock_skew − 2 × renew_interval`보다 짧으면 기한 전에 갱신이 다시
성공하므로 채널은 `failed`가 되지 않고 스스로 따라잡는다. 코드 기본값으로 23초, 참조
솔루션 설정으로 8초다. 갱신 요청이 걸리는 시간만큼 더 짧아질 수 있다. 참조 솔루션 설정에서
8초 장애가 유실 없이 복구된 기록이 있다([README 검증된 동작](../README.md#검증된-동작)).

**조치**

1. 장애 중에는 수집 서버를 재시작하지 않는다. 아직 `failed`가 아닌 채널의 스풀이 사라진다.
   이미 `failed`인 채널은 재시작해도 잃을 것이 없지만, S3가 응답하기 전에는 새 프로세스도
   `waiting_ownership`에서 기다릴 뿐이다.
2. 재생 서버도 재시작하지 않는다. 인덱스는 S3가 돌아오면 다음 패스에서 따라잡는다.
3. S3가 정상으로 돌아오면 `failed` 채널 수를 센다.

   ```sh
   curl -fsS "$TRANSMUX/channels" | jq '[.channels[] | select(.state == "failed")] | length'
   ```

4. `failed` 채널이 많은 샤드는 [샤드 재시작·교체](#샤드-재시작교체) 절차로 재시작한다.
   몇 대뿐이면 [failed 채널 복구](#failed-채널-복구)의 카메라 단위 방법을 쓴다.
5. 장애 구간은 녹화에 남지 않는다. 운영 기록에 구간을 남긴다.

**복구 확인**: 모든 샤드의 `/healthz`가 `ok`이고, 재생 서버 `/readyz`가 200이고,
`GET /v1/admin/status`의 `index.last_live_scan`이 최근 시각이다.

**에스컬레이션**: 장애가 반복되면 `lease.ttl`을 늘려 허용 장애 시간을 늘릴지 검토한다.
대신 강제 종료 시 인수 시간도 같이 늘어난다. 설정 검증은
`2 × renew_interval + 2 × operation_timeout + max_clock_skew < ttl`과
`operation_timeout < renew_interval`을 요구한다.

### ffmpeg이 반복 재시작되는 카메라

**증상**: 한 카메라가 `disconnected`, `reconnecting`, `starting`을 오가고 라이브가 자주 끊긴다.

**알람 조건**

```promql
# 30분에 5회 넘게 ffmpeg을 다시 띄웠다
increase(transmux_channel_reconnects_total[30m]) > 5

# 5분 넘게 단절·재연결 대기 상태다
(transmux_channel_state == 3) or (transmux_channel_state == 4)
```

**확인**

```sh
CENTER=center-01 CAMERA=cam-01
curl -fsS "$TRANSMUX/channels/$CENTER/$CAMERA" |
  jq '{state, reconnects, segments_lost, last_error, last_error_at, source_url, ffmpeg_pid}'
```

이 카메라의 로그(`center_id`, `camera_id` 필드)에서 다음을 찾는다.

| 로그 `msg` | 뜻 |
|---|---|
| `ffmpeg started` | 새 세대 시작. `generation`이 몇 번째인지 알려준다 |
| `stall detected, terminating ffmpeg` | `saw_segment: false`면 `startup_timeout` 안에 첫 세그먼트가 없었고, `true`면 `stall_timeout` 동안 새 세그먼트가 없었다 |
| `pipeline ended` | ffmpeg이 끝났다. `error`에 종료 이유가 있다 |
| `pipeline ended without error` | 원본 스트림이 끝났다(`ffmpeg exited with status 0 (source ended)`) |
| `reconnecting` | 다음 시도까지 `delay`를 기다린다. `attempt`는 연속 실패 횟수다 |
| `ffmpeg` | ffmpeg stderr 한 줄. `message`에 자격증명이 제거된 원문이 있다 |
| `ffmpeg stderr suppressed for the rest of this run` | 한 세대의 stderr가 너무 많아 나머지를 생략했다 |

**판단 기준**

- `attempt`가 n인 재연결 대기는 `[base / 2, min(base × factor^(n−1), max)]` 범위의 무작위
  값이다(`reconnect.*`). 기본으로 최대 30초다. 세그먼트를 `reconnect.reset_after`(기본 60초)
  이상 계속 저장해야 대기 시간이 초기화된다. 접속만 되고 바로 멈추는 카메라는 최대 대기
  시간 근처에서 계속 재시도한다.
- `pipeline ended`가 곧바로 반복되거나 `saw_segment: false`인 정체가 반복되면 RTSP 접속,
  인증, 코덱 설정 문제다. ffmpeg stderr의 `message`를 본다. `video_codec: "hevc"`를 H.264
  카메라에 지정하는 것처럼 실제 코덱과 다른 설정도 원인이 된다.
- `saw_segment: true`인 정체가 반복되면 카메라가 스트림 도중 멈춘다. 카메라 부하나
  네트워크 손실을 의심한다. GOP가 `stall_timeout`보다 길어도 같은 증상이 난다.
- 카메라 한 대의 반복 재시작은 같은 샤드의 다른 채널에 영향을 주지 않는다. `/livez`도
  영향받지 않는다.

**1차 조치**

1. 카메라 네트워크에서 RTSP 응답을 확인한다. 자격증명이 들어간 주소를 명령 인자로 쓰면
   셸 기록과 프로세스 목록에 남는다.
2. 카메라 쪽 원인이 확인되면 담당자에게 넘기고, 경보 소음이 크면 운영자 화면의
   **카메라 관리**에서 **수집 중지**를 누른다. 녹화와 목록 항목은 남는다.
3. 설정 문제면 운영자 화면의 **편집**에서 코덱, 형식, 음성 설정을 고친다. 주소, 형식, 음성,
   코덱을 바꾸면 다음 목록 갱신에서 채널이 다시 시작된다(`camera source changed, restarting channel`).
4. 샤드를 재시작하지 않는다. 같은 샤드의 정상 채널까지 끊긴다.

**복구 확인**: `reconnects`가 더 늘지 않고 `state`가 `receiving`, `healthy`가 참이다.

**에스컬레이션**: 같은 카메라 모델 여러 대에서 같은 stderr가 반복되면 ffmpeg 호환성 문제일 수
있다. 해당 `message`(주소 제거)와 카메라 모델을 붙여 버그 신고를 연다.

### failed 채널 복구

**증상**: `transmux_channel_state` = 7이거나 `transmux_channel_failed_total`이 늘었다.

```promql
# TransmuxChannelFailed
transmux_channel_state == 7
```

`failed`는 계속 발행하면 스트림을 망가뜨릴 수 있을 때 채널을 멈춘 상태다. 자동으로
재시도하지 않는다. 채널은 프로세스가 끝나거나, 카메라가 목록에서 빠지거나, 카메라의 주소·형식·
음성·코덱이 바뀔 때까지 그대로 남는다.

**확인**: 로그에서 해당 카메라의 마지막 `channel disabled: ...` 메시지를 찾는다.

| 로그 `msg` | 원인 | 복구 전에 할 일 |
|---|---|---|
| `channel disabled: ownership of the published stream was lost` | lease 갱신 거절이나 기한 초과 | S3 장애였으면 S3 복구 확인. 다른 샤드가 인수했으면 그쪽이 정상인지 확인 |
| `channel disabled: another shard owns the published stream` | `error`가 `camera lease lost`뿐이거나 `refusing to publish past the lease deadline`이 있으면 기한 초과, `another writer`나 `another owner`가 있으면 다른 기록자 발견 | 기한 초과면 위와 같다. 다른 기록자면 [lease 소유권 상실](#lease-소유권-상실)의 이중 기록 조사 |
| `channel disabled: the lease object is not usable` | lease 객체가 손상됐거나 다른 형식 | 객체를 보존용으로 복사한 뒤 원인 조사 |
| `channel disabled: cannot establish safe ownership of the published stream` | 매니페스트가 이 데몬의 형식이 아니거나 시퀀스를 복구할 수 없음 | 매니페스트를 지우거나 덮어쓰지 말고 개발팀에 넘긴다 |

마지막 경우에 매니페스트를 지우면 다음 프로세스가 시퀀스를 처음부터 다시 시작할 수 있다.
그러면 이미 재생 중인 플레이어와 캐시가 깨진다.

**조치**: 원인을 없앤 뒤 둘 중 하나로 채널을 다시 시작한다.

- **카메라 단위**(공유 목록 `object` 공급자): 운영자 화면 **카메라 관리**에서 **수집 중지**를
  누르고, 수집 서버 `/channels`에서 해당 채널이 사라진 것을 확인한 뒤(`cameras.poll_interval`
  이내), **편집**에서 **영상 수집 사용**을 다시 켠다. API로 할 때 `PUT /v1/admin/cameras/{center}/{camera}`는
  보낸 필드로 설정을 교체한다. `name`, `audio`, `format`, `video_codec`을 빠뜨리면 기본값이
  되므로 `GET /v1/admin/cameras`의 항목 전체를 보낸다.
- **샤드 단위**: [샤드 재시작·교체](#샤드-재시작교체)를 따른다. 그 샤드의 모든 채널이 다시
  시작된다. `static` 공급자는 이 방법만 쓸 수 있다.

**복구 확인**: 해당 채널이 `receiving`, `healthy: true`이고 `transmux_channel_lease_held` = 1이다.

**에스컬레이션**: 원인을 모르는 `failed`는 다시 시작하기 전에 로그(자격증명 제외)를 보존하고
개발팀에 넘긴다.

### 녹화 인덱스 불일치와 재구축

`playbackd`의 녹화 인덱스(`index_path`, 기본 `/var/lib/transmux/playback/recordings.db`)는
S3에서 다시 만들 수 있는 캐시다. S3가 원본이다. 인덱스를 잃어도 수집은 멈추지 않는다.

**증상**: S3에는 객체가 있는데 녹화 조회가 비어 있다. 또는 지운 녹화가 조회되고 재생하면
404가 난다.

**확인**

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" "$PLAYBACK/v1/admin/status" | jq .index
curl -fsS -G -H "Authorization: Bearer $TOKEN" "$PLAYBACK/v1/recordings" \
  --data-urlencode center_id=center-01 --data-urlencode camera_id=cam-01 \
  --data-urlencode start=2026-10-01T00:00:00Z --data-urlencode end=2026-10-01T06:00:00Z |
  jq '{index_complete, indexed_at, periods: (.periods | length)}'
```

**판단 기준**: 대부분은 재구축할 필요가 없다.

- `index_complete: false`면 그 날짜의 S3 목록을 아직 다 읽지 않았다. 백필이 진행 중이므로
  기다린다.
- 외부에서 지운 녹화는 다음 일일 대조에서 빠진다. 최대 약 24시간 걸린다
  ([솔루션 가이드](solution.md#api)).
- 조회 구간이 `index.days`보다 오래됐으면 S3에 있어도 조회되지 않는다. 정상 동작이다.
- `playbackd`가 `open recording index (one process per file): timeout`으로 기동하지 않으면
  다른 프로세스가 같은 파일을 열고 있다. 손상이 아니다. 같은 인덱스 파일을 쓰는 두 번째
  `playbackd`를 정지한다.
- 다음 경우에만 재구축한다. bbolt 파일을 열 수 없다(위 timeout 제외). 또는 `index.last_pass`가
  문제 발생 이후 시각인데도 S3와 계속 어긋난다.

**재구축이 미치는 영향**

- 재생 서버가 한 대뿐이므로 재시작하는 동안 재생과 관리 화면이 멈춘다. 수집은 계속된다.
- 진행 중인 녹화 재생 세션은 같은 파일에 저장돼 있으므로 무효가 된다. 시청자는 다시 연다.
- 보존 작업의 카메라별 커서가 초기화된다. 다음 보존 패스가 처음부터 다시 훑는다.
- 새 인덱스는 새 식별자를 가진다. 이전 식별자의 내보내기 임시 디렉터리는 자동으로
  정리되지 않는다.
- 백필은 창 안의 모든 세그먼트에 HEAD를 한 번씩 보낸다. 요청 비용은
  [용량 모델](capacity-model.md#s3-비용-요청-요금만)의 GET 단가로 계산한다.

**절차**

1. `playbackd`를 정지한다.
2. 인덱스 파일을 지우지 말고 옮겨 둔다. 새 인덱스가 검증될 때까지 보관한다.

   ```sh
   mv /var/lib/transmux/playback/recordings.db \
      /var/lib/transmux/playback/recordings.db.$(date -u +%Y%m%dT%H%M%SZ)
   ```

3. `export.temp_dir`(기본 `/var/lib/transmux/playback/exports`)의 `clip-*` 디렉터리를 지운다.
   `playbackd`가 정지된 동안에만 하고, 같은 디렉터리를 쓰는 다른 재생 서버가 없을 때만 한다.
4. `playbackd`를 시작한다. 최근 라이브 매니페스트는 `index.poll_interval`마다 바로 인덱싱된다.
5. 과거 날짜 백필을 지켜본다. 패스마다 카메라·날짜당 한 페이지(256개)씩 최근 날짜부터 처리하고,
   병렬도는 `max(1, index.workers / 4)`다(기본 2). 백필 시간은 대략
   `창 안의 세그먼트 수 × HEAD 지연 ÷ 병렬도`다. 더 빨리 끝내야 하면 `index.workers`를 높인다
   (허용 범위 2∼64).

**복구 확인**: `index.indexed_since_start`가 늘고, `index.failures_since_start`가 늘지 않고,
대표 카메라·날짜의 `/v1/recordings`가 `index_complete: true`다. 확인이 끝나면 옮겨 둔 파일을
지운다. 이 파일에는 카메라 정보와 세션 목록이 있으므로 영상 데이터와 같은 기준으로 지운다.

**에스컬레이션**: 재구축 뒤에도 `recording metadata rejected`가 계속 나오면 세그먼트의 S3
메타데이터가 잘못된 것이다. 해당 객체 키를 개발팀에 넘긴다.

### 롤백

릴리스 이미지는 `ghcr.io/jeonghun-app/transmux`에 `0.2.0`과 `v0.2.0` 같은 전체 버전 태그로
게시된다. 게시된 버전은 바뀌지 않는다. `latest`와 `0.2` 같은 짧은 태그는 움직이므로 롤백
대상으로 쓰지 않는다. 한 이미지에 `transmuxd`와 `playbackd`가 함께 들어 있다.

**판단 기준**: 새 버전 배포 뒤 위 알람이 새로 울리고, 설정이나 환경 변경으로 설명되지 않으면
롤백한다. 데이터 형식이나 설정의 호환성은 [CHANGELOG](../CHANGELOG.md)의 **호환성** 항목을 먼저
확인한다.

**절차**

1. 현재 버전과 되돌릴 버전을 기록한다.

   ```sh
   curl -fsS "$TRANSMUX/metrics" | grep '^transmux_build_info'
   ```

   `playbackd`는 기동 로그 `playbackd ready`의 `version` 필드로 확인한다.

2. 현재 설정이 이전 버전에서도 유효한지 확인한다. 두 데몬 모두 알 수 없는 설정 키가 있으면
   기동하지 않는다. 새 버전에서 추가한 키를 쓰고 있으면 이 단계에서 실패한다.

   ```sh
   docker run --rm -v "$PWD/config.json:/etc/transmux/config.json:ro" \
     ghcr.io/jeonghun-app/transmux:<previous-version> -validate
   docker run --rm --entrypoint /usr/local/bin/playbackd \
     -v "$PWD/config.json:/etc/transmux/config.json:ro" \
     -v "$PWD/playback.json:/etc/transmux/playback.json:ro" \
     ghcr.io/jeonghun-app/transmux:<previous-version> -validate
   ```

3. 재생 서버를 되돌리기 전에 `playbackd`를 정지하고 인덱스 파일을 복사해 둔다. 이전 버전이
   새 버전의 인덱스를 제대로 쓰지 못하면 [녹화 인덱스 불일치와 재구축](#녹화-인덱스-불일치와-재구축)으로
   다시 만든다.
4. 수집 서버는 [샤드 재시작·교체](#샤드-재시작교체) 절차로 샤드마다 교체한다. lease를 지원하는
   버전끼리는 롤링 교체해도 이중 기록이 생기지 않는다. lease를 모르는 구버전으로는 롤링 교체하지
   않는다. 이전 프로세스를 모두 멈춘 뒤 시작한다.

**복구 확인**: `transmux_build_info`의 `version`이 되돌린 버전이고, 알람이 해소되고, 재생 서버
`/readyz`가 200이다.

**에스컬레이션**: 롤백으로 해결되면 원인 버전과 증상을 버그 신고로 남긴다. 롤백 후에도 같으면
배포 외 원인이므로 해당 알람 절로 돌아간다.

## 에스컬레이션 경로

- 당직과 책임자 연락은 운영 조직의 체계를 따른다.
- 코드 결함으로 보이면 [버그 신고 양식](../.github/ISSUE_TEMPLATE/bug_report.yml)으로 이슈를 연다.
  버전, 설정(자격증명 제외), 관련 로그 `msg`, 메트릭 값을 붙인다.
- 보안 문제는 공개 이슈 대신 [보안 정책](../SECURITY.md)의 비공개 경로로 신고한다.
