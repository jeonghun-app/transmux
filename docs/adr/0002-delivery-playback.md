# ADR 0002 — 고객 디바이스 전달(delivery)·재생 경로

- 상태: **과거 제안, 현재 구현은 ADR 0003으로 대체**
- 날짜: 2026-09-01
- 선행: [ADR 0001](0001-architecture.md)
- 후속: [ADR 0003 — 카메라 관제·녹화 솔루션](0003-camera-solution.md), 2026-09-12

이 문서는 초기 CloudFront·DynamoDB 제안과 비용·지연 분석을 보존한다.
아래의 “현재”, “미구현”, “결정 대기”는 작성 당시를 뜻한다.
실제 구현은 인증 프록시, 영속 시간 인덱스, 카메라별 권한과 오디오·fMP4를
채택했다. 현재 API·IAM 계약은 [솔루션 가이드](../solution.md)와
[보안 정책](../../SECURITY.md)을 따른다.

## 1. 배경

ADR 0001과 그 구현은 RTSP를 받아 HLS로 트랜스먹싱해 S3에 넣는 **ingest 경로**만
다룬다. 고객 디바이스(관제 클라이언트, 웹, 모바일)가 그 스트림에 어떻게 접속해
재생하는지는 설계도 구현도 없었다.

현재 코드 확인 결과:

- `internal/storage`에 delivery용 presign, CloudFront, 인증 코드가 없다.
- `internal/health`의 `/channels`는 **인증 없는 운영 모니터링 API**이며 고객용
  카탈로그가 아니다.
- PoC에서 재생이 되는 것은 MinIO에 anonymous download를 켜뒀기 때문이다
  (`deploy/docker-compose.poc.yml`). 프로덕션 모델이 아니다.
- 라이브 윈도우(4~6개) 밖의 세그먼트는 S3에 남지만 어떤 매니페스트도 참조하지
  않는다. 즉 **저장은 되지만 재생할 수 없다.**

## 2. 결정: 기본 경로

```
고객 디바이스
  └─ ① 인증 → Playback API (센터/카메라 권한 확인)
       └─ ② 센터 범위 CloudFront signed cookie 발급
            └─ ③ CloudFront (cookie 검증, 세그먼트 캐시)
                 └─ ④ OAC → private S3
```

S3 측 요건:

- Block Public Access 4종 전부 활성화
- `s3:GetObject`는 CloudFront OAC principal에만 허용하고, bucket policy에서
  `AWS:SourceArn`으로 distribution을 한정한다
- transmuxd task role은 지정된 ingest prefix에 대한 `s3:PutObject`와,
  시퀀스 복구용 `s3:GetObject`만 갖는다 (§6 참고)
- 고객에게 S3 URL이나 AWS 자격증명을 주지 않는다

CloudFront를 고르는 이유는 **비용 절감이 아니다**(§7에서 계산). signed cookie
기반 인증, origin 비공개화, 세그먼트 fan-out 캐시, WAF·접속 로그가 목적이다.

### 내부망 대안

클라이언트가 전부 사내망/VPC workload라면 **인증 리버스 프록시 + S3 Gateway
Endpoint**가 두 자릿수 배 저렴하다(§7). 단 `hls.js`, AVPlayer, ExoPlayer는
SigV4를 수행하지 못하므로 **프록시가 필수**다. "private S3 직접 재생"은 성립하지
않는다. `aws:SourceVpce`만으로 접근을 허용하는 방식은 같은 endpoint를 쓰는 다른
workload와 테넌트 격리가 되지 않으므로 채택하지 않는다.

## 3. 결정: 인증은 signed cookie (signed URL 아님)

이것은 취향이 아니라 현재 구현이 강제하는 결과다.

`internal/channel/worker.go`가 매니페스트에 쓰는 세그먼트 URI는 상대 경로다.

```
2026/09/01/seg-000000021-1788222135839.ts
```

`index.m3u8?Policy=...&Signature=...`로 매니페스트만 서명해도 이 상대 URI는
쿼리스트링을 상속하지 않는다. 플레이어의 세그먼트 요청은 전부 403이 된다.
signed URL을 쓰려면 모든 세그먼트 URI를 완전한 signed URL로 다시 렌더링하거나
플레이어 custom loader가 매 요청에 토큰을 붙여야 한다.

signed cookie는 도메인과 경로에 걸리므로 매니페스트와 모든 세그먼트에 자동
적용된다. 16분할도 센터 cookie 하나로 커버된다.

cookie 속성:

```
Secure; HttpOnly; SameSite=None; Domain=.example.com; Path=/live/{center_id}/
```

검토했으나 채택하지 않은 것:

| 방식 | 판단 |
|---|---|
| CloudFront signed URL | HLS fan-out에 부적합. 상대 URI와 충돌 |
| 쿼리 토큰 | 상대 URI에 상속되지 않음 |
| `Authorization` 헤더 | hls.js custom loader로는 가능하나 AVPlayer/ExoPlayer 호환이 불균일 |
| CloudFront Functions | 단순 HMAC 검사만 가능. DB 기반 권한 조회 불가 |
| Lambda@Edge | 모든 매니페스트·세그먼트 GET마다 실행 시 고비용·고복잡도 |

권한은 **세션 발급 시 한 번** 확인하고, edge에서는 짧은 만료의 증명만 검증한다.
강제 로그아웃 요구가 있으면 cookie TTL을 짧게 하고 refresh를 쓴다.

테넌트 격리는 두 곳에서 강제한다.

1. Playback API: 로그인 사용자 → 허용 center/camera entitlement
2. cookie resource policy: 허용 센터 prefix 밖 접근 차단

## 4. 결정: 재생 URL과 API

고객 디바이스가 URL을 직접 조립하지 않는다.

```http
POST /v1/playback-sessions
Authorization: Bearer <customer-token>

{ "center_id": "center-01", "camera_ids": ["cam-01"], "mode": "live" }
```

응답은 `Set-Cookie`로 signed cookie를 내리고 본문에 재생 URL을 담는다.

```json
{
  "expires_at": "2026-09-01T01:00:00Z",
  "streams": [
    { "camera_id": "cam-01",
      "url": "https://stream.example.com/live/center-01/cam-01/index.m3u8" }
  ]
}
```

카메라 카탈로그는 별도 API가 필요하다(`GET /v1/centers`,
`GET /v1/centers/{id}/cameras`). **`internal/health`의 `/channels`를 재사용하면
안 된다** — 인증이 없고 운영 지표용이다.

**마스터 플레이리스트는 현재 불필요하다.** 단일 H.264 rendition이므로 미디어
플레이리스트 직접 URL이 맞다. main/sub를 ABR variant로 묶는 것은 두 스트림의
timestamp/GOP가 정렬되어 있지 않으면 전환 시 stall을 유발하므로, 멀티뷰는 sub,
확대는 main을 **명시적으로 선택**하는 방식을 권한다.

이 API는 transmuxd에 넣지 않는다. 기존 고객 인증 시스템과 붙어야 하고, ingest
데몬을 사용자 트래픽 장애 도메인에 묶으면 안 된다.

## 5. 지연 예산: 약 15~17초

4초 세그먼트 기준 측정 가능한 구성 요소:

| 구간 | 시간 |
|---|---|
| 카메라 인코더 + RTSP 버퍼 | 0.1~1초 |
| 현재 세그먼트가 닫힐 때까지 (평균) | 2초 (최대 4초) |
| 스풀 스캔 + S3 PUT | 0.5~1.5초 |
| **플레이어 live edge 오프셋** (hls.js `liveSyncDurationCount` 기본 3 × 4초) | **12초** |
| 디코드/렌더 | 0.2~0.5초 |
| **합계** | **약 15~17초** |

GOP 8초 카메라는 28~35초가 된다.

**`live_window`를 줄여도 지연은 거의 줄지 않는다.** `live_window`는 매니페스트가
보유하는 DVR 깊이이고, 지연을 지배하는 것은 플레이어의 live edge 오프셋이다.
`live_window`는 4 미만으로 내리지 않는 것이 좋다 — 일시적 네트워크 지연만으로
시청자가 윈도우 밖으로 밀려난다.

트랜스코딩 없이 도달 가능한 하한:

1. 카메라 GOP를 2초로 설정 (**이것이 필수 전제**, ADR 0001 §8)
2. `segment.target_duration`을 2초로 설정
3. 플레이어 `liveSyncDurationCount`를 2로 명시
4. 매니페스트 캐시 TTL 0~1초

→ **약 6~8초.** 그 이하는 LL-HLS나 WebRTC가 필요하고, LL-HLS는 partial
segment·blocking playlist reload를 지원하는 origin이 필요하므로 정적 S3
origin으로는 사실상 불가능하다.

**Wowza가 RTSP 직결이나 WebRTC로 1~3초를 제공하고 있었다면 S3 HLS는 대체가 되지
않는다.** 이것이 §9의 최우선 미결 항목이다.

## 6. 구현한 것 (이번 변경)

전달 경로 자체는 미결 항목에 막혀 있으나, **어떤 delivery 방식을 고르든 전제가
되는 것** 두 가지를 먼저 구현했다.

### 6.1 발행 시퀀스 복구 — CDN 도입의 전제조건

발견한 문제: 로컬 체크포인트(`internal/channel/checkpoint.go`)는 인스턴스에
있으므로 ECS/EKS 태스크 교체를 넘기지 못한다. 교체 후 시퀀스가 0부터 다시
시작하면

- `EXT-X-MEDIA-SEQUENCE`가 과거로 되돌아가 플레이어가 깨지고,
- 세그먼트에 `immutable` 캐시를 걸어놨으므로 키가 재사용되는 순간 CDN 엣지가
  옛 내용을 계속 서빙한다.

해결: 사용 가능한 로컬 체크포인트가 없으면 **오브젝트 스토어의 발행 매니페스트를
읽어** 시퀀스를 복구한다(`internal/hls/published.go`,
`Worker.recover`). 세 가지 결과로 갈린다.

| 상황 | 동작 |
|---|---|
| 매니페스트 있음 | 최대 시퀀스에서 이어가고 discontinuity 삽입 |
| 매니페스트 없음(404) | 신규 채널. 0부터 시작 |
| 읽기 실패 | **fail closed.** 채널을 `failed` 상태로 두고 ffmpeg을 띄우지 않는다 |

마지막 항목이 핵심이다. 이력을 모르는 상태에서 0부터 시작하면 시청 중인
세그먼트를 덮어쓸 수 있다. 채널 하나가 조용한 것은 운영자가 복구할 수 있지만,
시퀀스 되돌림은 전 시청자의 재생을 깨뜨린다. 우리 것이 아닌 매니페스트(URI
네이밍 불일치)도 오류로 처리한다 — 다른 writer의 스트림을 덮어쓸 수 있다.

PoC 실측: `state_dir`을 tmpfs로 둔 채 컨테이너를 삭제·재생성했을 때 시퀀스가
16 → 22로 전진하고 되돌아가지 않았으며, 중복 객체 키가 발생하지 않았다.

부수 효과로 **`state_dir`에 영속 볼륨이 필수가 아니게 되었다.** ECS/EKS에서
훨씬 다루기 쉽다. 영속 볼륨이 있으면 시작이 조금 빠를 뿐이다.

이 때문에 task role에 `s3:GetObject`가 필요해졌다(매니페스트 키 한정으로 좁힐 수
있다).

### 6.2 세그먼트 인덱싱 메타데이터 — VOD의 전제조건

세그먼트 PUT에 S3 user metadata를 붙였다.

```
x-amz-meta-sequence: 21
x-amz-meta-pdt-ms: 1788222135839
x-amz-meta-duration-ms: 4000
x-amz-meta-discontinuity: false
x-amz-meta-center-id: center-01
x-amz-meta-camera-id: cam-shortgop
```

**지금 넣어야 하는 이유:** 파일명의 `unixms`는 시작 시각만 준다. VOD 매니페스트에
반드시 필요한 **실제 duration과 discontinuity가 없다.** 다음 세그먼트의 timestamp
차이로 duration을 추정하면 카메라 단절 구간과 마지막 세그먼트에서 틀린다. 지금
기록하지 않으면, 오늘부터 VOD 구현 시점까지 저장된 모든 세그먼트가 정확히
인덱싱 불가능해진다(하루 27TB).

이 필드는 순수 추가이며 객체 키나 매니페스트를 바꾸지 않는다.

## 7. 비용

4Mbps 스트림 1개를 30일 상시 시청 = 1,296GB. 예시 단가(us-east-1, 무료 구간·세금
제외)이며 실제 견적은 프로덕션 리전으로 재계산해야 한다.

| 동시 stream-viewer | 월 데이터 | S3 직접(인터넷) | CloudFront | 프록시 + VPC Endpoint |
|---:|---:|---:|---:|---:|
| 10 | 13TB | ~$1,172 | ~$1,115 | ~$5 + 프록시 비용 |
| 100 | 130TB | ~$11,716 | ~$11,146 | ~$52 + 프록시 비용 |
| 관제자 10명 × 16분할 = 160 | 207TB | ~$18.7K | ~$17.8K | ~$83 + 프록시 비용 |

비교 기준: 저장 비용은 625채널 × 4Mbps 30일분 약 810TB에서
$0.023/GB-month 가정 시 약 $18.6K/월.

- **비용을 지배하는 것은 요청 요금이 아니라 데이터 전송량이다.**
- 인터넷 경유에서 CloudFront와 S3 직접의 차이는 5% 수준이다. CDN은 비용이 아니라
  인증·보안·캐시 때문에 고른다.
- **사내망 경로는 두 자릿수 배 저렴하다.** 클라이언트 위치가 비용에 가장 큰 변수다.

멀티뷰는 사람 수가 아니라 stream-viewer 수로 계산해야 한다. 관제자 10명이
16분할을 보면 160 stream이다.

## 8. 멀티뷰

1080p/4Mbps 기준:

| 화면 | 수신 대역폭 | 시간당 |
|---:|---:|---:|
| 1채널 | 4Mbps | 1.8GB |
| 16분할 | 64Mbps | 28.8GB |
| 25분할 | 100Mbps | 45GB |

16개 동시 1080p 하드웨어 디코딩은 GPU·OS·브라우저별 세션 한도가 달라 일반
요구사항으로 보장할 수 없다. 소프트웨어 fallback이 걸리면 CPU가 급증한다.

권고: 타일에는 카메라의 **second stream**(360p/0.5~1Mbps), 확대 시 main으로 전환.
트랜스코딩 금지와 충돌하지 않는다.

단, 전 카메라 main+sub를 상시 ingest하면:

- 논리 채널 640 → 1,280
- 메모리 고정비 14MiB × 1,280 ≈ **17.5GiB** (채널당 메모리는 비트레이트에
  둔감하므로 서브스트림도 거의 같은 비용, `docs/capacity-model.md`)
- PID·FD·PUT 수 거의 2배

서브스트림은 **관제 화면이 열릴 때만 on-demand로 수신**하는 것이 자원상 낫다.
동시 관제 화면 수가 전체 카메라 수보다 훨씬 작다면 그렇다.

객체 경로에 profile 차원(`{center}/{camera}/{profile}/...`)을 넣는 것은 URL이
외부 계약이 되기 전에 결정해야 한다. 요구사항이 명시한 경로와 달라지므로 §9의
결정 사항으로 둔다. **현재 구현은 profile 차원이 없다.**

## 9. 미결 항목 — 이것들이 다음 구현을 막고 있다

| # | 질문 | 무엇이 갈리는가 |
|---|---|---|
| 1 | **고객 디바이스가 무엇이고 접속 경로가 인터넷인가 사내망인가** | CloudFront vs 인증 프록시. 비용이 두 자릿수 배 차이 |
| 2 | **허용 지연이 몇 초인가** | 15초가 안 되면 HLS 단일 경로로는 대체 불가. 프로토콜 자체를 재검토해야 함 |
| 3 | 권한 단위가 센터인가 카메라인가 | 센터면 signed cookie로 단순, 카메라면 훨씬 복잡 |
| 4 | 녹화 구간 재생이 필수인가 | 필수면 VOD 인덱서가 별도 과제로 추가 |
| 5 | 멀티뷰 최대 분할 수와 동시 사용자 수 | 서브스트림 필요 여부, 채널 수 2배 여부 |
| 6 | 카메라 second stream 설정이 가능한가 | 멀티뷰 해법 성립 여부 |
| 7 | 카메라 GOP를 2초로 설정할 수 있는가 | 지연 하한 (ADR 0001 §8) |
| 8 | H.265 카메라가 있는가 | fMP4 프로파일 필요 여부 (ADR 0001 §9) |
| 9 | 오디오가 필요한가 | 현재 `-an`으로 제거 중 |

1번과 2번이 나머지를 좌우한다. 이 둘 없이 playback API를 코딩하는 것은 낭비다.

## 10. VOD 설계 (승인 대기)

`ListObjectsV2`로 푸는 것은 불가능하다. 카메라 하나의 하루가 4초 세그먼트로
약 21,600개이고, 특정 카메라·하루로 좁히면 List 22회로 가능하지만, 객체명이
`sequence-unixms` 순서라 **S3가 시간 범위를 서버 측에서 필터링할 수 없다.**
30분 구간도 하루 전체를 list한 뒤 걸러야 하며 다수 사용자·다수 날짜에는 못 쓴다.

권장 구조:

```
세그먼트 PutObject (메타데이터 포함 — §6.2에서 이미 구현)
  └─ S3 Event
      └─ recording-indexer (별도 서비스)
          └─ DynamoDB
```

```
PK = CENTER#{center}#CAMERA#{camera}#DAY#{yyyyMMdd}
SK = PDT#{unixms}#SEQ#{sequence}
속성: object_key, duration_ms, discontinuity, size
```

S3 Event는 중복·지연·순서 역전이 가능하므로 indexer는 idempotent해야 하고,
장애 시 S3 메타데이터를 source of truth로 재구축할 수 있어야 한다.

**transmuxd에 넣지 않는다.** ingest 데몬의 라이브 발행을 DynamoDB 장애에
종속시키면 안 된다.

경계 처리: 요청 구간이 세그먼트 경계와 일치하지 않으므로 첫 세그먼트는 요청
시각 이전 프레임을 포함한다. 프레임 단위 정확한 절단은 별도 remux가 필요하다.
자정 구간은 두 날짜 파티션을 조회해야 한다. 카메라 단절 구간은
`EXT-X-DISCONTINUITY`로 표현하고, 완료된 과거 구간에는 `EXT-X-ENDLIST`를 넣는다.

## 11. 리스크

- **매니페스트 캐시 오염**: CloudFront cache policy의 minimum TTL이 1초 이상이면
  origin의 `no-cache`보다 우선할 수 있다. 잘못 캐시된 `index.m3u8`은 전 시청자를
  동시에 멈춘다. `*.m3u8` behavior의 min/default TTL을 0으로 두고 error caching
  TTL도 0으로 둔다.
- **세그먼트 immutable 캐시와 키 재사용**: §6.1로 완화하고, 이후 lease + 매니페스트
  fence로 dual writer를 막았다(ADR 0001 §10, 구현됨). 세그먼트 PUT도 조건부 생성
  (`If-None-Match: *`)으로 바뀌어, 남의 객체가 있는 키는 덮어쓰지 않고 채널을
  정지시킨다. 남는 위험은 시스템 시계 되돌림이며, 이는 lease의 시계 오차 상한
  가정과 같은 뿌리다.
- **OAC는 시청자 인증이 아니다.** CloudFront→S3 구간만 인증한다. 시청자 인증은
  signed cookie가 담당한다. 이 둘을 혼동하면 버킷은 닫혀 있는데 스트림은 누구나
  볼 수 있는 상태가 된다.
- **CORS**: signed cookie를 쓰면 `Access-Control-Allow-Origin: *`와
  `credentials`를 함께 쓸 수 없다. 허용 origin을 정확히 지정하고, origin에 따른
  응답 캐시 오염을 막아야 한다.
- **매니페스트 폴링 부하**: 동시 재생 stream 수 ÷ target_duration. 관제자 10명 ×
  16분할 = 40 GET/s. 일부 플레이어는 target duration의 절반마다 폴링해 두 배가
  된다.
- **모니터링 API 노출**: `/channels`, `/metrics`를 고객 delivery 도메인에 함께
  노출해서는 안 된다.
- **네이티브 플레이어 cookie 전달**: API 응답의 cookie가 AVPlayer/ExoPlayer의
  미디어 요청 cookie jar로 실제 전달되는지 반드시 검증해야 한다.
