# Changelog

이 프로젝트의 주요 변경 사항을 기록한다. 형식은
[Keep a Changelog 1.1.0](https://keepachangelog.com/ko/1.1.0/)을 따르고,
버전은 [Semantic Versioning 2.0.0](https://semver.org/lang/ko/)을 따른다.
1.0.0 이전에는 마이너 버전 증가에 호환되지 않는 변경이 포함될 수 있으며,
그런 항목은 **호환성** 표시와 함께 옮겨 갈 방법을 적는다.

## [Unreleased]

## [0.2.0] - 2026-10-04

### Added

- 릴리스 파이프라인: `v*` 태그를 푸시하면 linux/amd64·arm64 정적 바이너리
  (`transmuxd`, `playbackd`) 묶음, `SHA256SUMS`, SPDX SBOM을 첨부한
  GitHub Release와 `ghcr.io/jeonghun-app/transmux` 멀티아키텍처 이미지를
  게시한다. 릴리스 노트는 이 파일의 해당 버전 절에서 가져온다. 태그는
  `vMAJOR.MINOR.PATCH[-PRERELEASE]` 형식만 받고, 이미 게시된 버전은 다시
  게시하지 않는다(수정은 다음 패치 버전으로 한다). `latest` 이미지 태그는
  정식 버전에만 붙는다.
- `make dist`(릴리스 묶음과 체크섬 생성), `make lint-actions`(컨테이너로
  actionlint 실행).
- 런타임 이미지에 OCI 라벨(`source`, `version`, `revision`, `licenses`
  등)을 붙인다.
- Apache-2.0 `LICENSE`, `NOTICE`, 제3자 고지, 기여 안내, 행동 강령, 이슈·PR
  템플릿, `CODEOWNERS`, 영어판 `README.en.md`.
- Dependabot 설정(Go 모듈, 브라우저 테스트 npm, GitHub Actions, Docker 기반
  이미지).

### Changed

- **호환성:** `transmuxd`의 `http_listen` 코드 기본값이 `:8080`에서
  `127.0.0.1:8080`으로 바뀌었다. 인증이 없는 `/channels`, `/metrics`가 기본으로
  외부에 열리지 않는다. 컨테이너에서 헬스 체크·메트릭을 밖으로 노출해야 하면
  설정에 `"http_listen": ":8080"`을 명시한다(저장소의 `configs/*.json` 예제는
  이미 명시한다).
- `key_prefix`를 설정 로드 시 한 곳에서 정규화한다. `/tenant/video/`처럼 앞뒤
  `/`가 있어도 수집·재생·인덱싱·보존·내보내기가 같은 객체 키를 쓴다.
- HTTP 카메라 공급자에서 `enabled=false`가 된 카메라는 수집 대상에서만 빠지고
  재생 카탈로그·권한·보존 정리에는 남아 과거 녹화를 계속 재생할 수 있다.
- 로그인 시도 제한을 계정별과 클라이언트별로 따로 적용한다. 클라이언트 주소는
  설정된 신뢰 프록시(CIDR)에서 온 요청에만 `X-Forwarded-For`를 사용하고,
  기본은 연결 주소다. 같은 프록시 뒤의 사용자들이 서로를 막지 않는다.
- `make build`가 `-X main.version`으로 버전을 넣고 빌드한 두 바이너리의 버전을
  출력한다. `VERSION` 기본값은 `git describe --tags --always --dirty`다.
- 런타임 이미지 빌드에서 Go 컴파일은 빌더의 네이티브 플랫폼에서 교차
  컴파일한다(멀티아키텍처 빌드 시 에뮬레이션으로 Go를 돌리지 않는다).
- CI: `push`는 `main`에서만 실행해 PR 브랜치의 중복 실행을 없앴다.
  PR에서도 의존성 취약점 검사(`make vuln`)와 워크플로 린트를 실행한다.
  모든 액션을 커밋 SHA로 고정했다.
- **호환성:** 재생 설정에서 `retention.enabled`가 켜져 있으면 `index.days`가
  `retention.days` 이상이어야 한다. 인덱스는 `index.days`보다 오래된 날짜를
  정리하므로, 더 짧으면 보존 중인 녹화가 조회되지 않는다. 기존 설정이 이 조건을
  어기면 `playbackd`가 기동 시 오류를 낸다. `index.days`를 늘린다.
- 로컬 Compose 스택(solution·PoC)의 S3 서버를 MinIO에서 RustFS 1.0.1로 바꿨다.
  MinIO 공개 이미지를 더 이상 익명으로 받을 수 없기 때문이다. 서비스 이름은
  `s3`, 예제 설정의 endpoint는 `http://s3:9000`, 저장 볼륨은 `s3-data`다.
  `.env.solution`에는 `TRANSMUX_S3_SECRET_KEY`가 자동으로 추가된다. 이전 로컬
  녹화는 새 스택에서 보이지 않는다(정리 방법은 `docs/solution.md`).
- 로그인 제한의 최종 정책: 실제 계정과 존재하지 않는 이름(비밀 키 HMAC으로 고정
  4096개 버킷) 모두 실패한 시도만 1분에 10회로 센다. 검증 전에 시도를 먼저
  세고 성공하면 되돌려 동시 요청에서도 상한을 지킨다. 클라이언트는 1분에
  30회(IPv6는 /64), 추적 키가 10,000개를 넘으면 새 클라이언트는 공용 overflow
  한도(1분 300회)를 함께 쓰며, `GET /v1/admin/status`의 `login`과 경고 로그로
  관측한다.
- CI가 문서의 상대 링크와 앵커를 검사한다(`docs/check-links.py`).
- `SECURITY.md`를 취약점 신고 정책과 지원 버전으로 줄이고, 배포 하드닝 내용은
  `docs/deployment-security.md`로 옮겼다. 문서에서 특정 고객·사업 맥락을
  일반적인 서술로 바꿨다.

### Fixed

- 녹화 인덱스 순회(`ScanDay`)가 제한 시간이나 중간 오류로 끝나도 그때까지
  확인한 객체를 반영하고 커서를 전진시킨다. 느린 저장소에서도 순회가 진척된다.
- S3 Lifecycle 등 외부에서 삭제된 세그먼트가 재순회 후 인덱스와
  `/v1/recordings`에서 사라지며, 사라진 객체로 새 재생 세션을 발급하지 않는다.
- 인덱스가 보존 기간을 넘은 파티션을 정리해 무한히 커지지 않는다. 보존 정리는
  카메라 간 공정성을 유지하면서 배치 예산을 끝까지 사용한다.
- 여러 카메라를 한 번에 요청한 재생 세션 생성이 일부 실패하면 앞서 만든
  스냅샷과 세션 슬롯을 남기지 않는다.
- 동시 MP4 내보내기가 디스크 공간을 미리 예약한다. 합계가 가용량을 넘으면 뒤
  작업은 시작 전에 명확한 오류로 거절되고, 인덱스·세션 DB용 최소 여유 공간을
  남긴다.

- 프록시가 `X-Forwarded-For`에 `addr:port`나 `[v6]:port`를 붙여도 클라이언트
  주소를 읽는다. 전에는 그런 프록시 뒤의 모든 사용자가 프록시 주소 하나의
  로그인 한도를 함께 썼다.

## [0.1.0] - 2026-10-04

태그 이전 개발 기록을 묶은 첫 기준 버전이다.

### Added

- `transmuxd`: 카메라 채널마다 ffmpeg 프로세스 하나(`-c:v copy`)를 Go
  supervisor가 관리하며 RTSP를 HLS로 변환하고, tmpfs 스풀을 거쳐
  S3 호환 저장소에 올린다. 재연결, 시퀀스 복구, 헬스 체크, Prometheus 메트릭.
- H.264/MPEG-TS와 H.264·H.265/fMP4 출력, 카메라별 음성 제외·복사·AAC 변환.
- 단일 기록자 보장: 객체 저장소의 조건부 쓰기(`If-None-Match`/`If-Match`)로
  카메라 소유권 lease와 매니페스트 fence를 구현해, 재배포 경합이나 분할된
  shard가 한 카메라를 동시에 게시하지 않는다.
- `playbackd`: 센터·카메라 권한을 검사하는 인증된 라이브·녹화 재생, 녹화
  인덱스, 카메라 관리, MP4 내보내기, 운영자 화면.
- 실행 환경: PoC 스택(MediaMTX 모의 카메라, MinIO), 솔루션 스택
  (`make solution-up`), 용량 측정 스크립트, 아키텍처·전달 ADR.
- CI: 포맷·vet·단위·race 테스트, ffmpeg 통합 테스트, 설정 스키마 검사,
  솔루션 스택과 Chromium 브라우저 검사. 야간 PoC 종단 검사와 취약점 검사.

### Fixed

- 시작 시 게시된 매니페스트와 체크포인트 중 높은 시퀀스를 따라 크래시 후
  `EXT-X-MEDIA-SEQUENCE`가 되감기지 않는다. 매니페스트 PUT 실패를 기억해 다시
  시도하고, 세대 종료 시 남은 세그먼트를 한 번 더 올린 뒤 손실은 카운터로
  드러낸다. 빈 fence 매니페스트 뒤에도 시퀀스 하한이 유지된다.
- 조건부 쓰기를 재시도하지 않고 다시 읽어 모호함을 해소해, 응답 유실이 소유권
  상실로 오판되지 않는다.
- 설정 검증 강화(음수·비정상 기간, 필드 간 제약, 뒤따르는 JSON 문서 거부,
  `manifest_name` 경로 제한), 모니터링 리스너 바인드 실패를 치명 오류로 처리,
  URL 로그에서 자격 증명과 쿼리 제거.
- 업로드 슬롯을 먼저 얻고 세그먼트를 읽어 힙 사용량이 채널 수가 아니라 동시
  업로드 수에 비례한다. 재연결 중에도 정체 지표가 갱신된다.

[Unreleased]: https://github.com/jeonghun-app/transmux/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/jeonghun-app/transmux/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/jeonghun-app/transmux/releases/tag/v0.1.0
