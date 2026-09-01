# 보안 정책

## 취약점 신고

GitHub의 private vulnerability reporting을 사용해 주세요. 공개 이슈로 올리지
말아 주십시오. 버그 바운티는 없고, 대응은 best-effort입니다.

## 배포 시 반드시 알아야 하는 것

이 데몬에는 **의도적으로** 인증이 없는 표면이 있습니다. 설계상의 선택이며,
운영 환경이 그 전제를 지켜야 합니다.

### 모니터링 API는 인증이 없다

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

`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`는 **MinIO·LocalStack 개발용으로만**
읽습니다(`internal/storage/s3.go`). 프로덕션은 ECS task role 또는 EKS IRSA를
사용하고 이 변수를 비워 두십시오.

필요한 권한은 ingest prefix의 `s3:PutObject`와 매니페스트 키의 `s3:GetObject`
뿐입니다. `ListBucket`, `DeleteObject`, 버킷 전체 읽기 권한은 필요하지 않습니다.

### 전달 경로는 이 리포지토리에 없다

시청자 인증, CDN, 재생 URL 발급은 구현되어 있지 않습니다
([docs/wowza-coverage.md](docs/wowza-coverage.md)). **이 데몬이 쓰는 S3 버킷을
공개로 열어 매니페스트를 서비스하지 마십시오.** PoC 스택이 MinIO에 익명 다운로드를
허용하는 것은 검증 편의를 위한 것이며, 해당 compose 파일에 그렇게 명시돼 있습니다.

### 컨테이너

런타임 이미지는 non-root(UID 10001)로 실행되며 tini를 PID 1로 씁니다. PoC compose는
read-only 루트 파일시스템, `cap_drop: ALL`, no-new-privileges, PID 제한을 적용합니다.
프로덕션 매니페스트에도 같은 설정을 적용하는 것을 권장합니다.

## 지원 범위

이 프로젝트는 1.0 이전이며 전달 계층이 없습니다. 최신 `main`만 지원합니다.
