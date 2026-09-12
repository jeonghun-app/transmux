# 보안 정책

## 취약점 신고

GitHub의 private vulnerability reporting을 사용해 주세요. 공개 이슈로 올리지
말아 주십시오. 버그 바운티는 없고, 대응은 best-effort입니다.

## 배포 시 반드시 알아야 하는 것

수집 서버의 내부 모니터링 API와 재생 서버의 인증된 사용자 API를 분리합니다.
운영 환경에서도 두 서버의 네트워크 접근 범위를 구분해야 합니다.

### transmuxd의 모니터링 API는 인증이 없다

`/livez`, `/readyz`, `/healthz`, `/channels`, `/channels/{center_id}/{camera_id}`,
`/metrics`는 인증을 요구하지 않습니다(`internal/health/handler.go`).

- **노출되는 것**: 카메라 인벤토리(center/camera 식별자), 채널 상태, ffmpeg PID,
  자격증명이 제거된 소스 URL.
- **노출되지 않는 것**: RTSP 자격증명. userinfo와 쿼리스트링을 모두 제거하며
  (`internal/camera/camera.go`) 테스트로 강제합니다.
- **요구되는 조치**: `http_listen`을 내부 인터페이스에 바인딩하거나
  (`"127.0.0.1:8080"`) security group / service mesh 뒤에 두십시오. 공개 인터넷과
  카메라 네트워크에 노출해서는 안 됩니다.

liveness가 카메라·오브젝트 스토어 상태를 반영하지 않는 것도 의도된 것입니다.
카메라 한 대의 장애로 컨테이너가 재시작되면 같은 샤드의 나머지 채널까지 끊기고,
부분 장애가 전체 장애로 커집니다.

### 카메라 자격증명

RTSP URL에 담겨 전달되며, ffmpeg의 argv로 들어갑니다. 같은 PID 네임스페이스에
접근할 수 있는 프로세스는 `/proc`을 통해 이를 볼 수 있습니다. 컨테이너의 PID
네임스페이스와 호스트 접근 권한을 그에 맞게 통제하십시오.

로그·메트릭 라벨·API 응답·ffmpeg stderr에서는 마스킹됩니다.

### AWS 자격증명

AWS SDK의 기본 자격증명 체인을 사용합니다(`internal/storage/s3.go`). 프로덕션은
ECS task role 또는 EKS IRSA를 권장합니다. MinIO·LocalStack 개발에서는 환경 변수
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`를 사용할 수 있으며, 임시 자격증명을
제공할 때는 `AWS_SESSION_TOKEN`도 보존됩니다.

필요한 권한은 ingest prefix의 `s3:PutObject`와, 매니페스트·lease 키 **및 세그먼트
키**의 `s3:GetObject`입니다(`HeadObject`도 `s3:GetObject`로 인가됩니다). 세그먼트
읽기 권한이 필요한 이유는 조건부 생성이 거부됐을 때 그 객체가 우리 자신의 재시도인지
남의 것인지 `HEAD`로 확인해야 하기 때문입니다. 매니페스트만 읽을 수 있는 정책이면
프로덕션에서 충돌 처리가 실패합니다.
없는 키를 권한 오류와 구분하려면 버킷의 `s3:ListBucket`도 필요합니다. 이 권한이
없으면 AWS S3는 존재하지 않는 키의 GET에도 `403`을 반환할 수 있으므로 신규
채널의 lease·매니페스트 생성이 대기 상태에 머물 수 있습니다. 데몬은 목록 API를
호출하지 않으며 객체 읽기·쓰기 권한은 ingest prefix로 제한할 수 있습니다.
수집에는 `DeleteObject`가 필요하지 않습니다 — lease 해제는 tombstone 덮어쓰기입니다.
`storage.tag_media`를 켜면 미디어 PUT의 `s3:PutObjectTagging` 권한도 필요합니다.
`object` 카메라 공급자를 사용하면 버킷 루트의 `cameras.object_key`에도
별도 읽기 권한이 필요합니다. 해당 키는 미디어의 `storage.key_prefix`를 포함하지 않습니다.

`playbackd`는 미디어 읽기와 백그라운드 인덱싱을 위한 `GetObject`·`ListBucket`을
사용합니다. 자체 카메라 관리를 쓸 때만 공유 목록에 `PutObject`를 허용하고,
보존 삭제를 켤 때만 날짜별 미디어에 `DeleteObject`를 추가합니다.
수집·재생 역할을 분리하고 재생 서버에 lease·라이브 매니페스트 쓰기 권한을 주지
않으십시오. 공유 목록의 최초 생성은 관리자 또는 초기 수집 역할이 수행할 수 있습니다.

소유권 lease는 `{prefix}/{center_id}/{camera_id}/_transmux/lease.json`에
저장되며 shard 라벨과 랜덤 세션 식별자만 담습니다. RTSP URL·자격증명·호스트명은
들어가지 않습니다. **S3 lifecycle 규칙은 `_transmux/`와 라이브 매니페스트를 반드시
제외해야 합니다.** 활성 lease를 지우면 강제 failover가 발생합니다.

손상된 lease, 지원하지 않는 schema, 누락된 ETag, 잘린 매니페스트는 소유권의
근거로 채택하지 않습니다. 읽기 응답 크기 제한을 넘으면 앞부분만 파싱하지 않고
전체 요청을 실패시킵니다.

### playbackd의 인증과 미디어 전달

API는 HS256 Bearer JWT의 issuer·audience·발급/만료 시각·종류를 검증하고
센터·카메라·작업 권한을 확인합니다. 사용자용 API 토큰과 짧은 미디어 capability는
서로 다른 audience를 사용합니다. 매니페스트뿐 아니라 미디어·초기화 객체,
HEAD·Range·MP4 다운로드도 인증합니다. 미디어 경로로 제어 객체에 접근할 수 없습니다.

비밀번호는 환경변수에서 읽어 salt와 PBKDF2-SHA256 600,000회로 처리합니다.
로그인 요청과 동시 검증 수를 제한하지만 MFA·외부 IdP·즉시 토큰 폐기 목록은
제공하지 않습니다. 서명 키를 공유하는 중앙 백엔드가 권한 발급의 신뢰 경계입니다.
키를 브라우저나 개별 테넌트에 제공하지 마십시오. 기본 API 토큰 8시간·미디어
15분이며 권한 회수는 기존 토큰 만료 또는 서명 키 교체 시 반영됩니다.

`/media/{capability}/...`와 `/exports/{capability}/...`는 만료 전까지 접근 권한을
담은 URL입니다. 프록시 액세스 로그·분석 도구에서 해당 경로를 마스킹하십시오.
응답에는 `private, no-store`, `Referrer-Policy: no-referrer`를 적용합니다.
운영자 화면은 토큰을 메모리에만 보관합니다. 프록시에서는 HTTPS를 종단하고
`public_url`과 CORS 허용 origin을 정확히 지정해야 합니다.

**S3 버킷을 공개로 열어 매니페스트를 서비스하지 마십시오.**
솔루션 Compose의 MinIO 버킷은 비공개이며 저장소 포트를 호스트에 공개하지 않습니다.
기존 수집 전용 PoC는 검증 목적으로 익명 다운로드를 사용하므로 운영 전달 구성으로
사용해서는 안 됩니다.

### 카메라 목록과 로컬 상태

공유 목록에는 실제 RTSP 자격증명이 저장됩니다. 해당 S3 객체의 읽기 권한을
수집·관리 서비스로 제한하고 버킷 암호화·백업 접근 권한을 관리하십시오.
관리 API는 자격증명을 반환하지 않으며 ETag 조건부 쓰기로 동시 변경을 보호합니다.
`manage` 권한은 서버가 연결할 RTSP 목적지를 바꿀 수 있으므로 신뢰하는 운영자에게
부여하고 카메라 네트워크의 egress 범위도 제한하십시오.

녹화 인덱스에는 카메라 정보·캡처 시각·고정 세션 목록이, 내보내기 디렉터리에는
실제 영상이 저장됩니다. 재생 서버의 영속 디스크와 백업도 영상 저장소로 취급하십시오.
내보내기는 크기·시간·동시성 상한을 적용하며 직접 만든 로컬 파일만 ffmpeg에
전달합니다. 취소·종료·재시작 시 해당 인덱스 소유의 임시 파일을 정리합니다.

### 컨테이너

런타임 이미지는 non-root(UID 10001)로 실행되며 tini를 PID 1로 씁니다. PoC compose는
read-only 루트 파일시스템, `cap_drop: ALL`, no-new-privileges, PID 제한을 적용합니다.
프로덕션 매니페스트에도 같은 설정을 적용하는 것을 권장합니다.

## 지원 범위

이 프로젝트는 1.0 이전입니다. 최신 `main`만 지원하며 기능·검증 범위는
[Wowza 대체 범위](docs/wowza-coverage.md)와 [솔루션 가이드](docs/solution.md)에
명시합니다.
