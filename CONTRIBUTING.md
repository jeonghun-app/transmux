# 기여 가이드

버그는 [버그 신고 양식](.github/ISSUE_TEMPLATE/bug_report.yml), 기능 제안은
[기능 제안 양식](.github/ISSUE_TEMPLATE/feature_request.yml)으로 등록한다. 보안 취약점은
공개 이슈 대신 [보안 정책](SECURITY.md)의 비공개 신고 경로를 사용한다.
기여할 때는 [행동 강령](CODE_OF_CONDUCT.md)을 따른다.

## 개발 환경

- Docker와 Docker Compose v2, Python 3.9 이상이 필요하다.
- 브라우저 테스트에는 Node.js 24가 필요하다.
- Go와 ffmpeg는 호스트에 설치할 필요가 없다. Make 대상은 Go 도구와 ffmpeg를
  컨테이너에서 실행한다. 실행 명령과 이미지 버전은 [Makefile](Makefile)을 따른다.

## 작업 흐름

1. 관련 이슈를 확인하거나 작업 범위를 설명하는 이슈를 만든다.
2. 변경 범위가 분명한 브랜치에서 작업한다.
3. 변경과 관련된 테스트·문서를 함께 갱신한다. 한국어 문서는 서술형 `-다` 체를
   사용하고, [영문 README](README.en.md)와 [한국어 README](README.md)의 대응 내용은
   함께 맞춘다. 코드 주석은 영어로 작성한다.
4. 커밋 제목은 실제 변경을 설명하는 영어 명령형으로 쓴다. `Fix ...`, `Add ...`처럼
   작성할 수 있으며 Conventional Commits 접두사를 강제하지 않는다.
5. PR에는 관련 이슈, 변경 범위, 검증 결과, 문서·API 영향과 남은 제한을 적는다.
   실행하지 못한 검증은 명시한다. 테스트를 약화하거나 건너뛰어 통과시키지 않는다.

비밀 값, 실제 RTSP URL, JWT 서명 키, 액세스 키, 인증 토큰, 영상 데이터는 커밋,
이슈, PR, 로그 예시에 넣지 않는다. 설정과 로그를 공유할 때는 식별자와 자격증명을
가린다. `.env.solution` 같은 로컬 비밀 파일을 커밋하지 않는다.

## 검증

Go 변경에는 [CI](.github/workflows/ci.yml)와
[야간 검사](.github/workflows/nightly.yml)의 다음 명령을 사용한다.

```sh
make fmt-check vet test race build vuln
make test-ffmpeg image
```

ffmpeg를 실제로 사용하는 테스트는 `make test-ffmpeg`로 실행한다. 이 대상은
`deploy/Dockerfile`의 `test` 이미지를 빌드하고 `ffmpeg` 빌드 태그 테스트를 실행한다.
의존성 검사는 `make vuln`로 수행하며 야간 워크플로에도 포함된다.

설정 파일을 바꾸면 CI의 `-validate` 검사와 같이 이미지 빌드 뒤 PoC와 솔루션의
두 서비스를 확인한다.

```sh
make image
docker run --rm -v "$PWD/configs:/c:ro" transmux:latest \
  -config /c/poc.json -validate
docker run --rm \
  -v "$PWD/configs/solution-ingest.json:/etc/transmux/config.json:ro" \
  transmux:latest -validate
docker run --rm --entrypoint /usr/local/bin/playbackd \
  -v "$PWD/configs/solution-ingest.json:/etc/transmux/config.json:ro" \
  -v "$PWD/configs/solution-playback.json:/etc/transmux/playback.json:ro" \
  transmux:latest -validate
```

통합 동작과 운영자 화면을 바꾸면 솔루션 스택과 브라우저 검사를 실행한다.

```sh
make solution-up
python3 scripts/verify-solution.py --restart
cd tests/browser
npm ci
npx playwright install --with-deps chromium
npm test
```

수집 전용 PoC를 바꾸면 `make poc-up`, `make poc-verify`, `make poc-down`을 사용한다.
용량 측정은 전용 호스트에서 관련 변경을 검토할 때 수행한다. 문서만 변경한 PR은
관련 링크와 내용 검증으로 범위를 정할 수 있으며, 실행하지 않은 전체 검사는 PR에
정확히 적는다.

Markdown을 바꾸거나 문서를 이동하면 저장소 루트에서
[링크 검사 스크립트](docs/check-links.py)를 실행한다. 상대 경로와 Markdown 제목·앵커를
확인하며 외부 사이트의 가용성은 검사하지 않는다.

```sh
python3 docs/check-links.py
```

## DCO 서명

모든 커밋에는 [Developer Certificate of Origin 1.1](https://developercertificate.org/)
서명 문구 `Signed-off-by: 이름 <이메일>`이 필요하다. 이는 기여물을 제출할 권리가
있다는 확인이다. Git에 올바른 이름과 이메일을 설정한 뒤 다음처럼 커밋한다.

```sh
git commit -s -m "Add contribution guide"
```

기존 커밋을 수정할 때도 각 커밋의 서명을 유지한다.
