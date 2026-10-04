# transmux — IP 카메라 라이브·녹화 솔루션

**카메라 관제·녹화에 사용하는 Wowza 경로를 대체한다.** `transmuxd`가
RTSP 영상을 HLS로 저장하고, `playbackd`가 인증된 라이브·녹화 재생,
카메라 관리, MP4 내보내기와 운영자 화면을 제공한다.

H.264/MPEG-TS와 H.264·H.265/fMP4를 지원한다. 영상은 재인코딩하지 않고
복사한다(`-c:v copy`). 음성은 카메라별로 제외, 복사 또는 AAC 변환을 선택한다.
비공개 S3의 영상은 센터·카메라 권한을 검사하는 재생 서버를 통해 전달한다.

```
IP 카메라 ──RTSP/TCP──> transmuxd ──> 비공개 S3
                          │               │
                    소유권·재연결         ├──> 녹화 인덱스
                                          │        │
                                          └──> playbackd ──> 관제 화면·클라이언트
                                               인증·HLS·MP4
```

채널마다 ffmpeg 프로세스 하나를 Go supervisor가 관리한다. 근거와 대안
검토는 [docs/adr/0001-architecture.md](docs/adr/0001-architecture.md),
실측 용량은 [docs/capacity-model.md](docs/capacity-model.md).

실행·API·배포 계약은 [솔루션 가이드](docs/solution.md),
실제 대체 범위와 남은 조건은 [Wowza 대체 범위](docs/wowza-coverage.md),
현재 전달 구조의 결정은 [ADR 0003](docs/adr/0003-camera-solution.md)에 정리했다.
WebRTC·RTMP·SRT·ABR 등 방송 서버의 모든 기능을 제공하는 제품은 아니다.

## 빠른 시작

Docker/Compose v2와 Python 3가 필요하다. 호스트에 Go나 ffmpeg을 설치할 필요는 없다.
빌드·검사는 Go 1.26.8 / Alpine 3.23, 런타임은 Alpine 3.23을 사용한다.

```bash
make solution-up      # 비공개 저장소 + 수집·재생 서버 + 모의 카메라 3대
make solution-verify  # 인증, 라이브·녹화, Range, MP4 실제 디코딩
make solution-down    # 중지; 녹화·설정 볼륨은 보존
```

운영자 화면은 `http://localhost:8090`이다. `admin`과 `viewer`의 비밀번호는
자동 생성된 `.env.solution`에 있다. 계정·포트 설정과 브라우저 검증은
[솔루션 가이드](docs/solution.md#로컬-실행)를 참고한다.

개발 검사:

```bash
make help          # 사용 가능한 타겟
make fmt-check     # Go 포맷 검사
make vet           # 정적 검사
make test          # 단위 테스트
make race          # 동시성 검사
make vuln          # 모듈 무결성 및 도달 가능한 Go 취약점 검사
make test-ffmpeg   # 실제 ffmpeg이 필요한 통합 테스트
make image         # 런타임 이미지 빌드
```

기존 수집 전용 PoC와 용량 측정 구성도 유지한다.

```bash
make poc-up        # 가짜 카메라 2대 + MediaMTX + transmuxd + MinIO
make poc-up-capacity  # 위 + 용량 측정용 1080p/D1/720p 소스
make poc-verify    # 종단 검증 (재생 가능성, 매니페스트 정합성)
make poc-logs      # transmuxd 로그
make poc-down      # 정리
```

`make poc-up`은 실제 IP 카메라와 실제 S3 없이 전체 파이프라인을 돌린다.
MediaMTX가 카메라 역할을, MinIO가 S3 역할을 한다. 가짜 카메라 2대는
키프레임 간격이 다르다. `cam-shortgop`은 GOP 2초로 4초 목표를 지킬 수 있고,
`cam-longgop`은 GOP 8초로 지킬 수 없다. 둘 다 정상 동작해야 한다.

용량 측정:

```bash
make poc-up-capacity                              # 1080p/D1/720p 소스까지 포함
./scripts/measure-capacity.sh 2 10 25 50          # 메모리, FD, 프로세스 수
./scripts/measure-cpu.sh --profile hd 1 10 25 50  # CPU (cgroup 정밀 측정)
```

`measure-cpu.sh`는 `docker stats` 대신 cgroup v2 `cpu.stat`을 읽어 실제 소비된
CPU 시간을 측정하고, ffmpeg과 supervisor를 분리해 보여주며, 채널 수에 대한
기울기를 회귀로 구해 마진 비용을 낸다. 프로파일은 `shortgop`(GOP 2초),
`longgop`(GOP 8초), `d1`(704×480 0.5Mbps), `hd720`(720p 1Mbps),
`hd`(1080p 4Mbps)다. `d1`·`hd720`·`hd`는 `poc-up-capacity`로 띄운 소스가
필요하고, 스크립트가 측정 전에 실제 스트림을 프로브해 publisher가 없으면
거부한다 — publisher 없는 경로는 RTSP 계층에서 오류가 아니라 그냥 무음이므로
유휴 채널의 CPU를 측정한 값이 나오기 때문이다.

## 설정

수집과 재생은 각각 JSON 파일로 설정한다(`configs/solution-ingest.json`,
`configs/solution-playback.json`). 알 수 없는 키, 잘못된 카메라
식별자·RTSP URL, 중복 카메라, 잘못된 HTTP endpoint, 안전하지 않은 객체 prefix는
기동 전에 거부한다. `-validate`에도 동일한 검증을 적용한다.

```bash
transmuxd -config /etc/transmux/config.json -validate   # 설정만 검사
playbackd -config /etc/transmux/playback.json -validate
```

주요 항목:

| 키 | 의미 | 주의 |
|---|---|---|
| `max_channels` | 이 샤드가 받을 최대 채널 수 | 검증된 용량 이상으로 밀려들지 않게 하는 상한 |
| `segment.target_duration` | `-hls_time` 값 | **목표이자 하한.** 실제 길이는 카메라 GOP가 결정한다 |
| `segment.live_window` | 발행 매니페스트에 유지할 세그먼트 수 | |
| `segment.local_list_size` | ffmpeg `-hls_list_size` | 스풀에 세그먼트가 남아 있는 개수. `live_window`보다 커야 하며 검증에서 강제된다. 유예 시간은 이 값 × **실제** 세그먼트 길이(=카메라 GOP)이므로 target_duration으로 계산하면 과대평가된다 |
| `segment.max_gop_slack` | 누락 판정에 더하는 여유 | GOP가 긴 카메라를 고장으로 오인하지 않게 한다 |
| `ffmpeg.stall_timeout` | 세그먼트 미생성 시 ffmpeg 종료 임계 | 버전 독립적인 주 단절 감지기 |
| `ffmpeg.input_args` | `-i` 앞에 넣을 추가 인자 | 버전별 소켓 타임아웃 옵션을 넣는 자리 |
| `storage.endpoint` / `force_path_style` | MinIO/LocalStack용 | 프로덕션에서는 비운다 |
| `upload.max_concurrent` | 세그먼트·매니페스트 저장 요청 동시성 | 소유권 요청에는 별도로 `min(16, max_concurrent)`개의 슬롯을 예약해 업로드 적체가 lease 갱신을 막지 않게 한다 |
| `storage.tag_media` | 보존용 `transmux-kind` 객체 태그 | 기본 꺼짐. 켜면 `s3:PutObjectTagging` 필요; 제어 객체에는 태그를 붙이지 않는다 |
| `cameras.provider` | `static`, `http`, `object` | `object`는 조건부 갱신되는 공유 목록과 관리 API를 사용 |
| `cameras.shard_filter` | 담당 수집 서버 ID | 목록의 `shard_id`가 일치하는 카메라만 수집 |
| `cameras.static[].format` | `mpegts` 또는 `fmp4` | 기본 `mpegts`; HEVC에는 `fmp4` 권장 |
| `cameras.static[].video_codec` | `auto`, `h264`, `hevc` | HEVC는 `fmp4`와 함께 지정하면 Apple 호환 `hvc1` 표시 적용 |
| `cameras.static[].audio` | `none`, `copy`, `aac` | 기본 `none`; G.711 등은 `aac`로 변환 |
| `lease.ttl` | 소유권 만료 시간 | 장애 인수에는 TTL에 시계 오차 여유 `2×max_clock_skew`, 다음 로스터/lease 확인과 저장 요청 시간이 더해진다 |
| `lease.renew_interval` | 갱신 주기 | `ttl > 2×renew_interval + 2×operation_timeout + max_clock_skew`를 강제한다. 갱신 하나를 놓쳐도 다음 쓰기와 응답 유실 확인이 자기 마감 전에 끝나야 한다 |
| `lease.operation_timeout` | lease 요청 하나의 상한 | `upload.put_timeout`과 분리한다. lease 레코드는 수백 바이트, 세그먼트는 수 MB라 같이 묶으면 한쪽이 반드시 잘못 잡힌다 |
| `lease.max_clock_skew` | 샤드 간 시계 오차 상한 가정 | 인수자는 만료 후 이만큼 더 기다리고, 소유자는 이만큼 먼저 멈춘다. 호스트에 NTP가 필요하다 |

자격증명은 AWS SDK의 기본 체인으로 읽는다. 프로덕션은 ECS task role 또는 EKS
IRSA를 권장하며, MinIO 개발 환경은 `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`를
사용한다. 임시 환경 자격증명을 사용할 때는 `AWS_SESSION_TOKEN`도 함께 전달된다.

필요한 S3 권한은 ingest prefix의 `s3:PutObject`와 `s3:GetObject`다. 읽기 권한은
매니페스트·lease 및 세그먼트의 충돌 확인용 HEAD까지 포함한다. 또한 없는 키에 대한
GET을 `404`로 구분하려면 버킷의 `s3:ListBucket` 권한이 필요하다. 이 권한이 없으면
S3는 없는 키에도 `403`을 반환할 수 있고, 데몬은 새 카메라라고 추측하지 않고
소유권 확인을 기다린다. `transmuxd`는 목록 조회나 객체 삭제 API를 호출하지 않는다.
`playbackd`에는 인덱싱용 목록 권한이 필요하고, 보존 삭제를 켜면 미디어 삭제 권한도
필요하다. 역할별 권한은 [보안 정책](SECURITY.md)을 참고한다.

수집의 `state_dir`에 영속 볼륨은 **필요하지 않다** —
기동할 때마다 발행 매니페스트를 읽어 시퀀스를 확정하며, 로컬 체크포인트는
"이 값 이하로는 절대 내려가지 않는다"는 하한으로만 쓴다. 체크포인트는 매니페스트
PUT이 성공한 **뒤에** 기록되므로 그 사이에 죽으면 매니페스트보다 뒤처질 수 있고,
따라서 단독 권위가 될 수 없다. 두 값 중 큰 쪽을 채택한다.
재생의 녹화 인덱스·고정 VOD 목록에는 별도의 영속 디스크를 사용한다.

## 객체 레이아웃

```
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/seg-{sequence}-{unixms}.ts
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/seg-{sequence}-{unixms}.m4s
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/init-{sha256}.mp4
s3://bucket/{prefix}/{center_id}/{camera_id}/index.m3u8
s3://bucket/{prefix}/{center_id}/{camera_id}/_transmux/lease.json
s3://bucket/_transmux/rosters/{name}.json
```

세그먼트에는 S3 user metadata가 붙는다(`sequence`, `pdt-ms`, `duration-ms`,
`discontinuity`, `center-id`, `camera-id`). 실제 duration은 객체 키에서 복원할 수
없고 다음 세그먼트 timestamp로 추정하면 카메라 단절 구간에서 틀리기 때문에
업로드 시점에 기록한다. fMP4에는 `init-uri`도 기록한다.
`playbackd`의 백그라운드 인덱서가 이를 검증해 시간 인덱스를 만들고 복구한다.

세그먼트는 요구사항대로 날짜 디렉터리에 들어가고, 라이브 매니페스트는
카메라 루트에 둔다. 매니페스트를 날짜 디렉터리에 두면 자정마다 재생 URL이
바뀌기 때문이다. 이름은 `storage.manifest_name`으로 바꿀 수 있다.

날짜는 업로드 시각이 아니라 세그먼트 자신의 wall-clock 기준이다. 자정
직전에 촬영된 세그먼트가 업로드 지연 때문에 다음 날짜로 넘어가지 않는다.

## 수집 서버 모니터링

| 엔드포인트 | 용도 |
|---|---|
| `GET /livez` | 프로세스 생존만. 카메라나 S3 장애에 영향받지 않는다 |
| `GET /readyz` | 로스터 로드 완료 + 정상 채널이 절반 이상 (세그먼트 저장과 매니페스트 발행이 모두 최근이어야 정상) |
| `GET /healthz` | Zabbix/WhaTap용 집계. 항상 200이며 본문 필드로 알람 |
| `GET /channels` | 전체 채널 인벤토리 |
| `GET /channels/{center_id}/{camera_id}` | 개별 채널 |
| `GET /metrics` | Prometheus 텍스트 형식 |

채널 상태(`transmux_channel_state`): 1 starting, 2 receiving, 3 disconnected,
4 reconnecting, 5 stopping, 6 stopped, **7 failed**, 8 waiting_ownership.

`failed`는 자동 복구되지 않으며 운영자 개입이 필요하다 — 매니페스트가 우리 것이
아닐 때, 또는 다른 샤드에게 소유권을 빼앗겼을 때 진입한다.

`waiting_ownership`은 **정상적인 대기 상태다.** 다른 샤드가 아직 이 카메라를
소유하고 있거나(롤링 배포 중 정상), 오브젝트 스토어를 읽지 못해 소유권을 확정할 수
없는 경우다. 아무것도 발행하지 않고 ffmpeg도 띄우지 않으며, 조건이 해소되면 스스로
복구한다. 스토어 일시 장애로 75채널이 전부 운영자 개입을 요구하면 안 되기 때문에
`failed`와 구분한다.

## 다중 호스트 소유권

한 카메라는 반드시 한 샤드만 수신해야 한다. 두 샤드가 같은 매니페스트에 쓰면
플레이리스트가 두 타임라인을 왕복하고, 세그먼트는 1년 캐시 immutable이므로 키
재사용은 덮어쓰기가 아니라 **CDN 캐시 오염**이다.

오브젝트 스토어의 조건부 쓰기만으로 해결한다(별도 DB 없음).

1. **lease** — `{prefix}/{center_id}/{camera_id}/_transmux/lease.json`을
   `If-None-Match: *`로 생성하고 `If-Match`로 갱신한다. 살아 있는 lease를 다른
   샤드가 들고 있으면 ffmpeg을 띄우지 않는다.
2. **fence** — lease만으로는 부족하다. 얼어붙은 소유자가 깨어나 유효한 CAS 토큰을
   그대로 쓸 수 있기 때문이다. 그래서 새 소유자는 **ffmpeg을 켜기 전에** 방금 읽은
   버전을 조건으로 매니페스트를 다시 쓴다. 본문에 매 쓰기마다 다른
   `# transmux-write-id` 주석이 들어가므로 플레이리스트 내용이 같아도 ETag가
   바뀌고, 그 순간 이전 소유자의 토큰은 소멸한다. 소유권 이전 시점은 lease 획득이
   아니라 이 fence 쓰기다.
3. **세그먼트는 create-only** — `If-None-Match: *`. 키가 이미 있으면 우리 자신의
   재시도인지(`put-id` 일치) 남의 것인지 `HEAD`로 확인하고, 남의 것이면 덮어쓰지
   않고 채널을 멈춘다.

업로드 계층은 조건부 쓰기를 **자동 재시도하지 않는다.** 착지했는데 응답만 유실된 경우 재시도는 자기
전제조건이 이미 소비된 것을 보고 412를 받는데, 그건 "남에게 밀렸다"와 구별되지
않는다. 어떤 write ID를 썼는지 아는 호출자만 판단할 수 있으므로, 호출자가 읽어서
확인한다. 확인 요청까지 실패하면 원래 쓰기 식별자와 본문을 보관하고 다음 시도에서
먼저 다시 읽는다. 저장 버전이 그대로인 경우에만 같은 식별자로 다시 시도하고,
자기 쓰기가 확인되면 해당 ETag를 채택한 뒤 최신 매니페스트를 발행한다.

빈 윈도우로 fence를 쓰는 경우가 있다(체크포인트 floor가 매니페스트보다 앞설 때
윈도우를 버린다). 그래서 매니페스트 본문에 `# transmux-last-sequence`로 지금까지
발행한 최대 시퀀스를 남긴다. 이게 없으면 그 fence가 `EXT-X-MEDIA-SEQUENCE:0`을
발행하고, 나중에 그것만 보고 복구한 프로세스가 시퀀스를 0에서 다시 시작해 이미
살아 있는 객체 키를 재사용한다.

`# transmux-write-id`와 `# transmux-last-sequence`는 **형식 인식이지 인증이
아니다.** 버킷에 쓸 수 있는 주체는 어차피 세그먼트를 참조하는 매니페스트도 위조할
수 있다. 신뢰 경계는 IAM과 CAS이고, 이 주석들은 ETag를 바꾸고 자기 쓰기를
식별하기 위한 것이다.

`lease.renew_interval`마다 채널당 작은 PUT 하나가 추가된다. 625채널·5초 주기면
약 125 PUT/s로, 세그먼트·매니페스트 250 PUT/s에 더해진다.

배포 주의: lease를 모르는 구버전 바이너리는 조건 없이 쓰기 때문에 신·구 혼재
상태를 이 메커니즘으로 안전하게 만들 수 없다. 구버전 writer를 먼저 정지시켜야 한다.
S3 lifecycle 규칙은 `_transmux/`와 라이브 매니페스트를 **반드시 제외해야** 한다.

`/livez`가 카메라·S3 상태를 반영하지 않는 것은 의도적이다. 카메라 한 대나
S3 일시 장애로 liveness를 실패시키면 오케스트레이터가 컨테이너를 재시작해
같은 샤드의 나머지 채널까지 끊는다. 부분 장애를 전체 장애로 키우는 셈이다.

**보안: 이 엔드포인트들은 인증이 없다.** 카메라 인벤토리와 채널 상태를
노출한다(자격증명과 userinfo 포함 RTSP URL은 노출하지 않는다). 내부
인터페이스에 바인딩하거나 security group / service mesh 뒤에 두어야 하며,
공개 인터넷이나 카메라 네트워크에 노출해서는 안 된다.

### 알람 설계

| 상황 | 신호 |
|---|---|
| 카메라 단절 | `transmux_channel_state` = 3 또는 4, `transmux_channel_reconnects_total` 증가 |
| 세그먼트 누락 | `transmux_channel_segment_gap_alarm` = 1 |
| 세그먼트 유실(업로드 전 회수) | `transmux_channel_segments_lost_total` 증가 |
| S3 장애 | `transmux_channel_segment_upload_failures_total` 증가 + `state` = 2(receiving) |
| 세그먼트는 저장됐지만 재생에 안 보임 | `transmux_channel_manifest_upload_failures_total` 증가, `seconds_since_manifest` 상승 |
| 스풀 고갈 임박 | `transmux_channel_spool_oldest_seconds`가 `local_list_size × 세그먼트 길이`에 접근 |
| 채널 영구 정지 | `transmux_channel_failed_total` 증가 (운영자 개입 필요) |
| 두 샤드가 한 카메라를 수신 | `transmux_channel_manifest_conflicts_total` 또는 `transmux_channel_segment_conflicts_total`이 0이 아니면 즉시 조사 |
| 소유권 이전 | `transmux_channel_lease_lost_total` 증가, 새 샤드에서 `transmux_channel_lease_held` = 1 |
| 롤링 배포 정상 진행 | `transmux_channel_lease_contended_total`이 잠시 증가한 뒤 멈춤 |
| 종료 시 flush 실패 | `transmux_channel_final_drain_failures_total` 증가 |
| 카메라가 PDT를 안 줌 | `transmux_channel_missing_pdt_total` 증가 (날짜 디렉터리가 업로드 시각으로 대체됨) |
| 카메라 장애와 S3 장애 구분 | `last_ffmpeg_segment_timestamp` vs `last_segment_timestamp` |

마지막 항목이 중요하다. ffmpeg은 세그먼트를 만들고 있는데 발행 시각만
멈춰 있으면 오브젝트 스토어 장애다. 둘 다 멈췄으면 카메라 장애다.

`segments_published_total`은 **세그먼트 객체 저장 성공** 시점에 오른다. 그
세그먼트가 플레이어에 보이려면 매니페스트가 갱신되어야 하고, 그 시각은
`transmux_channel_last_manifest_timestamp_seconds`가 알려준다. 세그먼트 PUT은
되는데 매니페스트 PUT만 실패하는 상태가 실제로 존재하므로 두 신호를 분리해서
본다. `upload_failures_total`은 둘의 합계로 남겨두었다.

`seconds_since_segment`, `seconds_since_manifest`, `segment_gap_alarm`은 스크랩
시점에 계산한다. 저장된 게이지로 두면 재연결 backoff 중이나 `failed` 상태에서
갱신이 멈춰, 정작 필요한 순간에 오래된 값을 보게 된다.
첫 미디어 매니페스트가 한 번도 성공하지 않았어도 `seconds_since_manifest`는
첫 세그먼트 저장 시각부터 증가한다. 세그먼트만 저장되고 매니페스트가 발행되지 않는
채널은 정상으로 보고하지 않는다.

HTTP 카메라 목록은 `StaticCamera` JSON 배열 계약을 따른다. 알 수 없는 필드,
8MiB를 넘는 응답, 뒤에 다른 문서가 붙은 응답, `null`은 오류로 처리하고 기존
채널을 유지한다. 전체 카메라를 해제하려면 명시적으로 `[]`를 반환해야 한다.

## 알려진 제약과 미구현

- **대규모 장기 부하 미검증.** 기존 영상 복사 경로의 625채널 메모리 외삽은
  약 8.6GiB로 목표 8GB를 넘는다. 오디오 변환·인덱싱·시청·내보내기 부하는
  별도로 측정해야 한다. [용량 모델](docs/capacity-model.md) 참고.
- **일반 HLS 지연.** WebRTC·LL-HLS 및 1∼3초 저지연 전송을 제공하지 않는다.
- **HEVC 클라이언트 제약.** 실제 fMP4 수집·재생·MP4 디코딩을 검증했으나
  웹 재생은 OS·브라우저의 HEVC 디코더 지원에 달려 있다.
- **단일 재생 서버 기준.** 영속 bbolt 인덱스는 한 프로세스만 연다.
  여러 재생 서버 사이의 세션·내보내기 상태 공유와 자동 장애 전환은 없다.
- **CDN 배포는 별도.** 현재 전달은 인증 프록시다. CloudFront OAC·서명 쿠키,
  TLS 종단 및 고객 SSO 연동은 운영 환경에 맞춰 구성해야 한다.
- **녹화는 조각 경계 기준.** 프레임 단위 절단과 TS/fMP4 형식 변경을 가로지르는
  단일 VOD는 지원하지 않는다.
- **객체 경로에 profile 차원 없음.** 멀티뷰용 서브스트림(듀얼 스트림)을 쓰려면
  URL이 외부 계약이 되기 전에 결정해야 한다. ADR 0002 §8 참고.
- **HTTP provider는 고정 JSON 배열 계약만 지원한다.** 페이지네이션이나 별도 응답
  envelope를 쓰는 DB/API에는 어댑터가 필요하다.
- **보존 삭제 기본 꺼짐.** 백그라운드 정리와 태그 기반 S3 Lifecycle 예시를 제공한다.
  운영 보존 기간·처리율·버전 관리 정책을 적용해야 한다.

## 검증된 동작

PoC 스택에서 실제로 확인한 항목이다. 수집 장애 검증은
[품질 점검 기록](docs/quality-review.md), 재생·관리·내보내기 검증은
[솔루션 검증 기록](docs/solution-review.md)에 정리했다.

- 비공개 버킷에서 인증된 H.264 TS, H.264 fMP4, H.265 fMP4 라이브·녹화 재생
- AAC 복사·G.711→AAC 변환, HTTP Range, 실제 디코딩 가능한 MP4 추출
- 센터·카메라 범위, 관리·내보내기 권한, 토큰 만료·알고리즘·경로 제한
- 고정 녹화 세션, 자정·단절 처리, 인덱스 재시작·커서 복구
- 공유 카메라 목록의 동시 편집 충돌 방지, 담당 서버별 배정·수집 중지

- 라이브 RTSP → HLS → 오브젝트 스토어 파이프라인, ffmpeg 8.0.1
  (이전 검증 버전: 6.1.1/6.1.2)
- 세그먼트가 매니페스트보다 먼저 저장되고, 매니페스트가 참조하는 모든
  세그먼트가 존재함
- 발행된 스트림이 재생 가능하며 코덱이 h264 그대로임
- GOP 8초 카메라가 4초 목표에서 8초 세그먼트를 내놓고, 정상으로 판정됨
- 카메라 단절 시 지수 backoff(full jitter, `[base/2, min(base×factor^n, max)]`)로
  재연결, 시퀀스 되돌림 없음, `EXT-X-DISCONTINUITY` 삽입, 유실 0
- lease TTL 20초인 PoC에서 오브젝트 스토어 8초 중단 후 자동 복구,
  두 채널의 ffmpeg 재연결 0, 세그먼트 유실 0. 저장소 장애가 소유권 만료까지
  지속되면 채널은 `failed`가 되므로 TTL은 허용 장애 시간과 인수 지연을 함께
  고려해 설정한다
- SIGTERM 시 남은 세그먼트를 최대 15초 동안 flush 후 종료(그 안에 스토어가
  응답하지 않으면 남은 것은 유실로 로깅된다), 좀비 프로세스 0, exit code 0
- 카메라가 전혀 없는 상태에서 25채널을 띄워도 크래시 없음
- 컨테이너를 삭제·재생성해 로컬 체크포인트를 잃어도 발행 매니페스트에서 시퀀스를
  복구하고 되돌아가지 않음 (16 → 22, 중복 객체 키 0)
- 오브젝트 스토어를 읽을 수 없으면 아무것도 발행하지 않고 ffmpeg도 띄우지 않으며
  `waiting_ownership`으로 대기한다(로컬 체크포인트가 남아 있어도 동일). 매니페스트가
  우리 것이 아니면 `failed`로 fail closed 된다
- 두 데몬에 같은 카메라를 주면 한쪽만 ffmpeg을 띄우고 다른 쪽은
  `waiting_ownership`으로 대기. 소유자를 `docker pause`로 얼리면 TTL 20초 후
  대기하던 쪽이 인수하고 시퀀스는 되돌아가지 않음(457 → 459). 얼었던 쪽을 깨우면
  갱신 거부를 감지해 ffmpeg을 죽이고 `failed`로 가며 매니페스트를 건드리지 않음
- 채널당 CPU 0.37~0.83% 코어(0.5~4Mbps 실측 4점), 75채널까지 선형 확장,
  ffmpeg 81~84% / supervisor 16~19%
- 채널당 메모리 13.4~14.3 MiB로 비트레이트에 무관(0.5Mbps와 4Mbps가 동일)
