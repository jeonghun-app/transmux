# Wowza Streaming Engine 대비 커버리지

이 문서는 하나의 질문에만 답한다: **transmux가 Wowza Streaming Engine을 전부
대체할 수 있는가.**

결론부터: **아니다.** transmux는 Wowza의 **ingest 경로**를 대체한다. 시청자
디바이스가 닿는 모든 것 — 인증, CDN, 재생 URL 발급, 과거 영상 조회 — 은 구현되어
있지 않다.

## 출처 구분

- transmux 쪽 판정은 이 리포지토리의 코드를 읽고 파일 경로로 근거를 남겼다.
- **Wowza 쪽 기능 목록은 벤더 문서를 확인하지 않은 일반 지식이다.** LL-HLS,
  CMAF, WebRTC(WHIP/WHEP), SRT의 지원 범위와 DRM 파트너 목록, 라이선스 모델은
  버전에 따라 달라졌을 수 있다. 대외 문서에 인용하기 전에 벤더 문서로 확인할 것.
  다만 아래 결론은 transmux 쪽 공백에서 나오므로 Wowza 버전 차이에 영향받지 않는다.

## 판정 기준

| 표기 | 의미 |
|---|---|
| 대체됨 | 코드에 구현되어 있고 테스트 또는 PoC로 확인됨 |
| 부분 | 일부만 구현됨 |
| 설계만 | ADR에 설계가 있고 코드는 없음 |
| 없음 | 구현도 설계도 없음 |
| 불필요 | 이 워크로드(고정 IP 카메라, 관제, 녹화 보존, 내부·앱 시청자)에 해당 없음 |

## 1. Ingest

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| RTSP/RTP pull | **대체됨** | `internal/ffmpeg/command.go` `InputArgs`, `internal/camera/camera.go` `Validate` |
| RTSP over TLS (rtsps) | 부분 | 스키마는 허용하지만(`camera.go`) 테스트·PoC 경로에서 한 번도 검증하지 않았다 |
| RTSP 자격증명 처리 | **대체됨** | URL에 담고 로그·API·ffmpeg stderr 전부에서 마스킹(`camera.go` `SafeURL`, `StderrRedactor`) |
| RTSP announce / push ingest | 없음 | 리스너가 없다. NAT 뒤 카메라가 밀어넣는 구성이면 문제가 된다 |
| RTMP / RTMPS push | 불필요 | 카메라는 RTMP를 쓰지 않는다 |
| SRT | 없음 | 원격 사이트를 손실 WAN으로 받는 경우에만 의미 |
| WebRTC(WHIP) ingest | 불필요 | |
| MPEG-TS / multicast | 없음 | 카메라는 RTSP. 헤드엔드 장비가 멀티캐스트를 쏘는 구성이면 필요 |
| 동적 소스 로스터 | 부분 | `cameras.provider: http`가 스켈레톤이다(`camera.go` `HTTPProvider`) |
| ONVIF 디스커버리 | 없음 | Wowza도 하지 않는다. 다만 누군가는 해야 하고, 여기서는 로스터 제공자에게 미뤄져 있다 |

## 2. 트랜스코딩 / ABR

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| 트랜스코딩·트랜스레이팅 | **불필요 (설계상 배제)** | `-c:v copy`가 이 프로젝트의 전제다. ADR 0001 |
| ABR 래더, 렌디션 그룹 | 없음 | 마스터 플레이리스트를 만들지 않는다. ADR 0002 §4는 단일 렌디션에서 ABR이 불필요하고, main/sub 타임스탬프가 어긋나면 오히려 해롭다고 판단했다 |
| 하드웨어 인코딩 | 불필요 | |
| 오버레이·워터마크·디인터레이스 | 없음 → 불필요 | |
| **오디오 패스스루** | **없음** | `-an`으로 제거한다(`command.go`). AAC는 패스스루가 가능하지만 G.711은 MPEG-TS/HLS에 담을 수 없어 **트랜스코딩 없음 전제를 다시 열어야 한다.** 조건부 blocker |
| 카메라별 서브스트림(멀티뷰) | 설계만 | ADR 0002 §8. 현재 객체 키에 profile 차원이 없다 |

## 3. 패키징 / 전달

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| HLS(MPEG-TS) 라이브 | **대체됨** | `internal/hls/playlist.go` `RenderLive`, `deploy/verify.sh`로 종단 확인 |
| 롱 GOP에서 올바른 TARGETDURATION | **대체됨** | 실제 EXTINF 최대값의 ceil을 쓴다(`playlist.go`). PoC에서 GOP 8초 카메라로 확인 |
| 재연결 시 DISCONTINUITY / SEQUENCE | **대체됨** | `internal/channel/worker.go` |
| PROGRAM-DATE-TIME 기준 날짜 분류 | **대체됨** | 업로드 시각이 아니라 촬영 시각 기준(`worker.go` `publishSegment`) |
| LL-HLS | 없음 | ADR 0002 §5: 정적 S3 origin으로는 partial segment·blocking reload를 제공할 수 없다 |
| DASH / CMAF / fMP4 | 없음 | ADR 0001 §9에 여지만 남겨둠 |
| H.265 전달 | 부분·미검증 | `.ts`만 만든다. 범용 플레이어 호환은 보장 못 함. ADR 0001 §9 |
| RTMP / RTSP / WebRTC egress | 없음 | 지연 요구사항에 따라서만 의미가 생긴다 |
| **CDN·HTTPS 전달** | **설계만** | ADR 0002 §2 (CloudFront + OAC). `internal/storage`에 presign·CloudFront 코드가 없다 |
| CORS | 없음 | ADR 0002에 미결로 기재 |

## 4. 녹화 / DVR / VOD

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| 라이브 스트림 상시 보관 | **대체됨**(세그먼트 형태) | `worker.go`. 날짜 파티션은 촬영 시각 기준 |
| 나중에 색인할 수 있는 메타데이터 | **대체됨** | S3 user metadata에 sequence·pdt-ms·duration-ms·discontinuity 기록. duration은 키에서 복원 불가하므로 지금 써두는 것이 맞다 |
| nDVR / 타임시프트 | 부분 → 설계만 | `live_window`(기본 4~6 세그먼트) 안에서만 되돌릴 수 있다 |
| **과거 구간 재생(VOD)** | **설계만** | ADR 0002 §10. 세그먼트는 S3에 남지만 어떤 매니페스트도 참조하지 않는다 |
| MP4 클립 내보내기(증적 제출) | 없음 | 두 ADR 어디에도 없다. 관제 업무에서는 보통 계약 항목이다 |
| **보존 기간 / lifecycle** | **없음** | 하루 수천만 객체가 무한 축적된다 |

## 5. 보안

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| **재생 인증 / 토큰 인증** | **설계만** | ADR 0002 §3(서명 쿠키). 서명 URL이 안 되는 이유는 세그먼트 URI가 상대 경로라서다 |
| DRM | 불필요 | 자사 관제 영상에 DRM은 맞는 통제 수단이 아니다 |
| HLS AES-128 / SAMPLE-AES 암호화 | 없음 | 규제 대상 영상이면 선택지로 남는다 |
| 전송 중 암호화(HTTPS) | 설계만 | CDN 또는 프록시가 담당(ADR 0002 §2) |
| 저장 시 암호화(SSE-KMS) | 없음 | `internal/storage/s3.go`에 SSE 파라미터가 없다. 버킷 기본 암호화에 의존하게 된다 |
| Geo/IP 차단 | 없음 → 불필요 | 내부 시청자. 필요하면 CDN/WAF 계층 |
| ingest 자격증명 위생 | **대체됨(더 철저함)** | userinfo·쿼리 토큰 모두 마스킹하고 테스트로 강제 |
| 모니터링 API 인증 | **없음(의도적)** | `internal/health/handler.go`. 내부 인터페이스 바인딩이 전제 조건이다 |

## 6. 운영

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| 관리 REST API | 부분(읽기 전용) | `internal/health/handler.go`. 채널 추가·삭제·재설정 API가 없고 설정 변경은 재시작이 필요하다 |
| Manager UI | 불필요 | Prometheus + 수집기 + 구조화 로그로 대체(ADR 0001) |
| 플러그인 SDK | 불필요 | 단일 목적 데몬이다 |
| StreamTargets(외부 CDN 재송출) | 없음 → 불필요 | 타사 VMS에 먹여야 한다면 필요 |
| 스트림 상태 감시·알람 | **대체됨(더 세분화)** | 카메라 장애와 스토어 장애 구분(`last_ffmpeg_segment_timestamp` vs `last_segment_timestamp`), 세그먼트 실패와 매니페스트 실패 분리는 기성품에서 얻기 어렵다 |
| 라이선스 비용 | **제거됨** | 라이선스 서버도 인스턴스당 과금도 없다 |

## 7. 클러스터링 / HA

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| Origin–edge, 라이브 리피터 | 불필요 → 설계만 | S3가 origin, CDN이 edge(ADR 0002 §2) |
| 소스 장애 조치 | 부분 | 채널별 재연결·full jitter backoff·fail-closed 복구는 있다. A/B 소스 전환은 없다 |
| **안전한 수평 샤딩** | **대체됨** | 오브젝트 스토어 조건부 쓰기 기반 lease + 매니페스트 fence(`internal/channel/lease.go`, `worker.go` `fenceManifest`). 두 데몬에 같은 카메라를 준 뒤 소유자를 얼려 인수·펜싱을 실측 확인했다. 단위 테스트는 stale writer 펜싱·애매한 쓰기·시퀀스 floor 보존을 덮지만, 이중 데몬 인수 시나리오는 아직 수동 절차다. README "다중 호스트 소유권" 참고 |
| 무중단 롤링 배포 | **대체됨** | 매니페스트 기반 시퀀스 복구로 태스크 교체가 안전하고, 영속 볼륨이 필요 없다 |

## 8. 캡션 / 메타데이터 / 썸네일

| Wowza 기능 | transmux | 근거 |
|---|---|---|
| CEA-608/708 → WebVTT/TTML | 없음 | `-c:v copy`이므로 SEI는 TS 안에 살아남을 것으로 **추정**하나, 선언도 변환도 하지 않고 테스트도 없다 |
| ID3 / AMF 타임드 메타데이터 | 없음 | |
| KLV / ONVIF 메타데이터 트랙 | 없음 | `-map 0:v:0`이 비디오 외 모든 스트림을 버린다 |
| 썸네일 / 프리뷰 | 없음 | 디코딩이 필요하므로 transmuxd 안에 넣으면 "트랜스코딩 없음"이 깨진다. 별도 서비스여야 한다. 카메라 목록 UI가 있다면 실질 요구사항일 가능성이 높다 |

## 프로덕션에서 Wowza를 내리기 전에 반드시 필요한 것

1. **전달 계층** — 시청자 인증, CDN 또는 인증 프록시, 재생 URL 발급. PoC에서
   재생이 되는 것은 MinIO에 익명 다운로드를 열어놨기 때문이다.
2. **지연 요구사항 확인** — 4초 세그먼트에서 약 15~17초, GOP 2초로 낮춰도 6~8초가
   하한이다. 기존 Wowza가 RTSP 직결이나 WebRTC로 1~3초를 주고 있었다면 이것은
   백로그 항목이 아니라 프로토콜 수준의 불일치다.
3. **과거 영상 조회** — 색인과 구간 매니페스트. 증적 제출이 범위라면 클립 내보내기까지.
4. **S3 lifecycle / 보존 정책** — 없으면 저장 비용이 먼저 문제가 된다.
5. **카메라 로스터 연동** — `provider: http`가 스켈레톤이다.
6. **메모리 예산** — 625채널 외삽 약 8.6GiB 대 목표 8GB. 예산을 올리거나 호스트당
   채널을 줄이는 결정이 필요하다.
7. **오디오**(필요하다면) — G.711이면 무트랜스코딩 전제를 다시 열어야 한다.
8. **H.265**(해당 카메라가 있다면) — fMP4 프로파일이 필요하다.

## 선택 사항

썸네일, 서브스트림 멀티뷰, MP4 클립 내보내기, AES-128 또는 SSE-KMS, DASH/CMAF,
LL-HLS, rtsps 실검증, 인증된 관리 API, 설정 핫리로드.

## 정말 필요 없는 것

RTMP/OBS ingest, WebRTC·RTMP egress(지연 요구사항이 없다면), DRM, 트랜스코더
템플릿과 오버레이, 소셜 플랫폼 재송출, Manager UI, 플러그인 SDK, 서버 사이드
편성, Smooth Streaming/HDS, Geo 차단, 인스턴스당 라이선스 관리.

## 외부 계약이 되는 지점 — 클라이언트가 붙기 전에 확정할 것

객체 키와 매니페스트 위치는 클라이언트가 한 번 의존하면 바꿀 수 없다.

- 세그먼트 키 템플릿과 매니페스트 경로(`worker.go` `objectKey`).
- **`internal/hls/published.go`의 정규식이 이 레이아웃을 하드코딩한다**
  (`^\d{4}/\d{2}/\d{2}/seg-(\d+)-(\d+)\.ts$`). 레이아웃을 바꾸면 기존 발행
  매니페스트가 전부 "우리 것이 아님"으로 판정되어 재시작 시 채널이 `failed`로
  간다. 레이아웃 변경에는 `ParsePublished`의 마이그레이션 경로나 명시적 컷오버가
  반드시 따라와야 한다.
- 세그먼트 URI가 상대 경로라는 점 — 서명 쿠키를 선택하게 만든 이유다.
- S3 user metadata 필드 이름 — 향후 색인기의 입력 스키마다.
- 세그먼트의 `immutable` 캐시 정책 — 키를 재사용하면 덮어쓰기가 아니라 CDN 캐시
  오염이 된다.
- profile 차원 도입 여부(ADR 0002 §8). 가장 깨질 가능성이 높은 지점이다.

## 지금 할 수 있는 정확한 주장

> transmux는 동작이 검증된 RTSP→HLS 트랜스먹싱 ingest·보관 데몬이다. 코덱을
> 건드리지 않고 MPEG-TS HLS로 패키징하며, 세그먼트를 매니페스트보다 먼저
> 저장하고, 카메라 단절과 오브젝트 스토어 장애를 유실 없이 통과하며, 컨테이너가
> 교체되어도 발행 시퀀스를 복구하고 되돌리지 않는다. 카메라 장애와 스토어 장애를
> 구분해 보고한다. 합성 PoC 스택에서 75채널까지 확인했고, 625채널 CPU는
> 3.6~5.2코어, 메모리는 약 8.6GiB로 외삽된다.

여기에 없는 것: 시청자 인증, CDN 전달, 재생 URL 발급, 과거 영상 재생, 오디오,
검증된 H.265, 보존 정책.
