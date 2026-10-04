# 서드파티 라이선스 고지

transmux의 소스는 [Apache License 2.0](LICENSE)으로 배포한다. 아래 구성 요소는
각 저작권자와 라이선스를 유지한다. 바이너리·이미지를 재배포할 때 이 문서와
[NOTICE](NOTICE), 해당 라이선스 전문을 함께 보존한다.

## Go 모듈

다음 표는 [go.mod](go.mod)의 직접 의존성 5개와 간접 의존성 16개 전체를 포함한다.
2026-10-04에 Docker 볼륨 `transmux-gomodcache`의 각 모듈에서 라이선스를 확인했다.
확인 경로는 `{GOMODCACHE}/{모듈}@{버전}/{확인 파일}`이다. 예를 들어 AWS SDK 본체는
`github.com/aws/aws-sdk-go-v2@v1.41.5/LICENSE.txt`다.

Apache-2.0 전문은 [LICENSE](LICENSE)에 있다. AWS SDK for Go의 `NOTICE.txt`와
Smithy Go의 `NOTICE`는 [프로젝트 NOTICE](NOTICE)에 원문 그대로 포함했다.
MIT와 BSD-3-Clause 전문·저작권은 아래에 보존한다.

### 직접 의존성

| 모듈 | 버전 | 라이선스 | 확인 파일 |
|---|---|---|---|
| `github.com/aws/aws-sdk-go-v2` | `v1.41.5` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/config` | `v1.28.6` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/s3` | `v1.97.3` | Apache-2.0 | `LICENSE.txt` |
| `github.com/golang-jwt/jwt/v5` | `v5.3.1` | MIT | `LICENSE` |
| `go.etcd.io/bbolt` | `v1.4.3` | MIT | `LICENSE` |

### 간접 의존성

| 모듈 | 버전 | 라이선스 | 확인 파일 |
|---|---|---|---|
| `github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream` | `v1.7.8` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/credentials` | `v1.17.47` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/feature/ec2/imds` | `v1.16.21` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/internal/configsources` | `v1.4.21` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | `v2.7.21` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/internal/ini` | `v1.8.1` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/internal/v4a` | `v1.4.22` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding` | `v1.13.7` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/internal/checksum` | `v1.9.13` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url` | `v1.13.21` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/internal/s3shared` | `v1.19.21` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/sso` | `v1.24.7` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/ssooidc` | `v1.28.6` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/aws-sdk-go-v2/service/sts` | `v1.33.2` | Apache-2.0 | `LICENSE.txt` |
| `github.com/aws/smithy-go` | `v1.24.2` | Apache-2.0 | `LICENSE` |
| `golang.org/x/sys` | `v0.44.0` | BSD-3-Clause | `LICENSE` |

## 브라우저 플레이어

[hls.js 1.7.3](https://github.com/video-dev/hls.js/tree/v1.7.3)은 Apache-2.0이다.
포함된 [hls.LICENSE.txt](internal/playback/web/hls.LICENSE.txt)에 Dailymotion과
파생 코드의 Brightcove 저작권·라이선스 고지를 보존한다.
배포 원본 URL, npm SHA-512 무결성 값과 `hls.min.js` 파일의 SHA-256은
[hls.VERSION.txt](internal/playback/web/hls.VERSION.txt)에 기록한다.

## 런타임 이미지와 ffmpeg

[Dockerfile](deploy/Dockerfile)의 런타임은 Alpine 3.23에서 `ffmpeg`, `tini`,
`ca-certificates`와 해당 패키지의 의존성을 설치한다. Go 서비스는 ffmpeg를 별도
프로세스로 실행한다. 이미지의 각 패키지는 자체 라이선스를 따르므로 이미지 전체에
프로젝트의 Apache-2.0만 적용되는 것은 아니다.

2026-10-04에 빌드한 이미지의 `ffmpeg 8.0.1-r1`을 확인했다.
Alpine 패키지 메타데이터는 `GPL-2.0-or-later AND LGPL-2.1-or-later`를 표시하며,
실제 `ffmpeg -L`은 **GPL-3.0-or-later**를 표시한다.
패키지 메타데이터와 실제 바이너리의 표시가 다른 이유는 `ffmpeg -buildconf`에서
확인한 `--enable-gpl --enable-version3` 등의 빌드 옵션이 GPL 코드와 버전 3 조건을
활성화하기 때문이다.
GPL 적용 여부와 버전은 배포하는 바이너리의 빌드 옵션·링크된 라이브러리로 확인한다.

- FFmpeg 저작권: `Copyright (c) 2000-2025 the FFmpeg developers`.
- 라이선스·재배포 안내: [FFmpeg legal](https://ffmpeg.org/legal.html),
  [GNU GPL v3 전문](https://www.gnu.org/licenses/gpl-3.0.html).
- Alpine 패키지의 대응 소스·패치·빌드 설정:
  [aports 3.23-stable의 ffmpeg](https://gitlab.alpinelinux.org/alpine/aports/-/tree/3.23-stable/community/ffmpeg).
  `APKBUILD`가 지정하는 원본은 [ffmpeg-8.0.1.tar.xz](https://ffmpeg.org/releases/ffmpeg-8.0.1.tar.xz)다.
- `tini`, `ca-certificates`와 전이 패키지도
  [Alpine aports](https://gitlab.alpinelinux.org/alpine/aports)에서 라이선스·소스를 확인한다.

이미지를 재배포할 때 GPL 구성 요소와 공유 라이브러리의 저작권·라이선스 사본,
해당 버전의 대응 소스와 패치·빌드 스크립트 제공 의무를 함께 충족해야 한다.
Alpine 브랜치와 패키지는 갱신되므로 배포 이미지의 digest, 패키지 버전과
대응 aports 커밋·소스 아카이브를 함께 보존한다. 다음 명령으로 실제 이미지를 확인한다.

```sh
docker run --rm --entrypoint ffmpeg transmux:latest -L
docker run --rm --entrypoint ffmpeg transmux:latest -buildconf
docker run --rm --entrypoint sh transmux:latest -c 'apk info -a ffmpeg'
```

## MIT·BSD 라이선스 원문

아래 전문은 위 모듈 캐시의 `LICENSE`에서 복사했다.

### github.com/golang-jwt/jwt/v5

```text
Copyright (c) 2012 Dave Grijalva
Copyright (c) 2021 golang-jwt maintainers

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and associated documentation files (the "Software"), to deal in the Software without restriction, including without limitation the rights to use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

```

### go.etcd.io/bbolt

```text
The MIT License (MIT)

Copyright (c) 2013 Ben Johnson

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
```

### golang.org/x/sys

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

### Go 런타임과 표준 라이브러리

바이너리에 포함되는 Go 런타임·표준 라이브러리는 BSD-3-Clause를 따른다.
Go 1.26.8 빌드 이미지의 `/usr/local/go/LICENSE` 전문이다.

```text
Copyright 2009 The Go Authors.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are
met:

   * Redistributions of source code must retain the above copyright
notice, this list of conditions and the following disclaimer.
   * Redistributions in binary form must reproduce the above
copyright notice, this list of conditions and the following disclaimer
in the documentation and/or other materials provided with the
distribution.
   * Neither the name of Google LLC nor the names of its
contributors may be used to endorse or promote products derived from
this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
"AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
(INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
