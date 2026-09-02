# transmux — RTSP → HLS 경량 트랜스먹싱 모듈

**Wowza Streaming Engine의 ingest 경로를 대체한다.** IP 카메라의 RTSP(H.264)를
**트랜스코딩 없이** HLS 세그먼트로 잘라 S3에 저장한다. 코덱은 건드리지 않는다
(`-c:v copy`).

전달·재생 경로(CDN, 시청자 인증, 재생 URL 발급, 과거 영상 조회)는 구현되어 있지
않으므로, 이것만으로 Wowza를 전부 대체할 수는 없다. 무엇이 대체되고 무엇이
남았는지는 [docs/wowza-coverage.md](docs/wowza-coverage.md)에 기능별로 정리했다.

```
IP 카메라 ──RTSP/TCP──> ffmpeg (stream copy) ──> tmpfs 스풀 ──> Go 업로더 ──> S3
                            ▲                                      │
                            └────── Go supervisor ─────────────────┘
                                    (상태·재연결·헬스체크)
```

채널마다 ffmpeg 프로세스 하나를 Go supervisor가 관리한다. 근거와 대안
검토는 [docs/adr/0001-architecture.md](docs/adr/0001-architecture.md),
실측 용량은 [docs/capacity-model.md](docs/capacity-model.md).

**이 모듈은 ingest 경로다.** 고객 디바이스가 여기에 어떻게 접속해 재생하는지
(CDN, 인증, 재생 URL, 과거 영상 조회)는 아직 구현되어 있지 않다. 설계와 남은
결정 사항은 [docs/adr/0002-delivery-playback.md](docs/adr/0002-delivery-playback.md).

## 빠른 시작

호스트에 Go나 ffmpeg을 설치할 필요는 없다. 모든 작업은 컨테이너에서 돈다.

```bash
make help          # 사용 가능한 타겟
make test          # 단위 테스트
make test-ffmpeg   # 실제 ffmpeg이 필요한 통합 테스트
make image         # 런타임 이미지 빌드

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

JSON 파일 하나로 설정한다(`configs/poc.json` 참고). 알 수 없는 키가 있으면
기동을 거부하므로 오타가 조용히 기본값으로 넘어가지 않는다.

```bash
transmuxd -config /etc/transmux/config.json -validate   # 설정만 검사
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
| `cameras.provider` | `static` 또는 `http` | `http`는 외부 DB/API에서 동적 로드 |
| `lease.ttl` | 소유권 만료 시간 | 죽은 샤드의 카메라를 다른 샤드가 인수하기까지의 최악 지연 |
| `lease.renew_interval` | 갱신 주기 | TTL은 갱신 2회 + 시계 오차 2배보다 커야 하며 검증에서 강제된다 |
| `lease.max_clock_skew` | 샤드 간 시계 오차 상한 가정 | 인수자는 만료 후 이만큼 더 기다리고, 소유자는 이만큼 먼저 멈춘다. 호스트에 NTP가 필요하다 |

자격증명: 프로덕션은 ECS task role 또는 EKS IRSA를 쓴다.
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`는 MinIO 개발용으로만 읽는다.

필요한 S3 권한은 ingest prefix에 대한 `s3:PutObject`와, 시퀀스 복구를 위한
매니페스트 키의 `s3:GetObject`다. `state_dir`에 영속 볼륨은 **필요하지 않다** —
기동할 때마다 발행 매니페스트를 읽어 시퀀스를 확정하며, 로컬 체크포인트는
"이 값 이하로는 절대 내려가지 않는다"는 하한으로만 쓴다. 체크포인트는 매니페스트
PUT이 성공한 **뒤에** 기록되므로 그 사이에 죽으면 매니페스트보다 뒤처질 수 있고,
따라서 단독 권위가 될 수 없다. 두 값 중 큰 쪽을 채택한다.

## 객체 레이아웃

```
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/seg-{sequence}-{unixms}.ts
s3://bucket/{prefix}/{center_id}/{camera_id}/index.m3u8
```

세그먼트에는 S3 user metadata가 붙는다(`sequence`, `pdt-ms`, `duration-ms`,
`discontinuity`, `center-id`, `camera-id`). 실제 duration은 객체 키에서 복원할 수
없고 다음 세그먼트 timestamp로 추정하면 카메라 단절 구간에서 틀리기 때문에,
나중에 녹화 인덱스를 만들 수 있도록 업로드 시점에 기록한다.

세그먼트는 요구사항대로 날짜 디렉터리에 들어가고, 라이브 매니페스트는
카메라 루트에 둔다. 매니페스트를 날짜 디렉터리에 두면 자정마다 재생 URL이
바뀌기 때문이다. 이름은 `storage.manifest_name`으로 바꿀 수 있다.

날짜는 업로드 시각이 아니라 세그먼트 자신의 wall-clock 기준이다. 자정
직전에 촬영된 세그먼트가 업로드 지연 때문에 다음 날짜로 넘어가지 않는다.

## 모니터링

| 엔드포인트 | 용도 |
|---|---|
| `GET /livez` | 프로세스 생존만. 카메라나 S3 장애에 영향받지 않는다 |
| `GET /readyz` | 로스터 로드 완료 + 정상 채널이 절반 이상 (degraded가 과반이면 실패) |
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

## 알려진 제약과 미구현

- **오디오 미지원.** 현재 `-an`으로 제거한다. HLS/MPEG-TS에서 AAC는
  가능하지만 G.711은 아니므로 카메라별 코덱 정책이 정해져야 한다.
- **H.265 미검증.** `.ts`만 생성한다. HEVC-in-TS는 생성은 되지만 범용
  플레이어 호환을 보장할 수 없다. fMP4 프로파일이 필요할 수 있다.
  ADR 0001 §9 참고.
- **메모리 8GB / 625채널 미달.** 실측 외삽 약 8.6GiB.
  CPU는 충족한다(625채널 3.6~5.2코어). `docs/capacity-model.md` 참고.
- **고객 전달 경로 미구현.** CDN, 시청자 인증, 재생 URL 발급 API, CORS가 전부
  없다. ADR 0002 참고.
- **과거 영상 재생 미구현.** 라이브 윈도우 밖의 세그먼트는 S3에 남지만 어떤
  매니페스트도 참조하지 않는다. 인덱싱용 메타데이터는 이미 기록하고 있으므로
  별도 indexer를 붙이면 소급 적용이 가능하다. ADR 0002 §10 참고.
- **객체 경로에 profile 차원 없음.** 멀티뷰용 서브스트림(듀얼 스트림)을 쓰려면
  URL이 외부 계약이 되기 전에 결정해야 한다. ADR 0002 §8 참고.
- **`cameras.provider: http`는 스켈레톤.** 실제 DB/API 스키마에 맞춰야
  한다.
- **S3 lifecycle 정책 미정.** 없으면 하루 수천만 객체가 무한 축적된다.

## 검증된 동작

PoC 스택에서 실제로 확인한 항목이다.

- 라이브 RTSP → HLS → 오브젝트 스토어 파이프라인, ffmpeg 6.1.1/6.1.2
- 세그먼트가 매니페스트보다 먼저 저장되고, 매니페스트가 참조하는 모든
  세그먼트가 존재함
- 발행된 스트림이 재생 가능하며 코덱이 h264 그대로임
- GOP 8초 카메라가 4초 목표에서 8초 세그먼트를 내놓고, 정상으로 판정됨
- 카메라 단절 시 지수 backoff(full jitter, `[base/2, min(base×factor^n, max)]`)로
  재연결, 시퀀스 되돌림 없음, `EXT-X-DISCONTINUITY` 삽입, 유실 0
- 오브젝트 스토어 30초 장애 중 ffmpeg 재시작 없음, 복구 후 스풀에 남은
  세그먼트 전부 업로드, 유실 0. 재측정(35초 중단) 기록: 스풀 9파일 7.0MB까지
  누적, `spool_oldest_seconds` 34초, `seconds_since_manifest` 38초,
  `reconnects_total` 0, 복구 후 `segments_published_total` 53 → 70,
  `segments_lost_total` 0
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
