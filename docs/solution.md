# 카메라 관제·녹화 솔루션

`transmuxd`가 RTSP를 수집하고 `playbackd`가 인증, 라이브 전달, 녹화 조회,
카메라 관리, MP4 내보내기를 담당한다. 운영자 화면과 HLS 플레이어도
`playbackd` 바이너리에 포함된다. 카메라 수집은 재생 서버나 녹화 인덱스가
중단되어도 계속된다.

## 로컬 실행

필요한 도구: Docker/Compose v2, Python 3. Go와 ffmpeg는 컨테이너에서 실행한다.

```sh
make solution-up
make solution-verify
```

브라우저에서 `http://localhost:8090`에 접속한다.

| 계정 | 비밀번호 위치 | 권한 |
|---|---|---|
| `admin` | `.env.solution`의 `TRANSMUX_ADMIN_PASSWORD` | 전체 카메라, 녹화, 내보내기, 관리 |
| `viewer` | `.env.solution`의 `TRANSMUX_VIEWER_PASSWORD` | 모의 정문·창고 카메라의 라이브·녹화 |

`make solution-env`는 비밀번호와 서명 키를 생성하고 기존 파일은 보존한다.
이 파일은 Git과 Docker 빌드 컨텍스트에서 제외된다. 파일 권한은 `0600`이다.
`make solution-down`은 컨테이너를 중지하되 녹화·카메라 목록·인덱스 볼륨을 보존한다.

모의 카메라는 다음 경로를 실제 RTSP로 전송한다.

| 카메라 | 입력 | 저장 | GOP |
|---|---|---|---|
| 정문 `entrance` | H.264 + AAC | MPEG-TS, 음성 복사 | 2초 |
| 창고 `warehouse` | H.264 + G.711 μ-law | fMP4, 음성 AAC 변환 | 8초 |
| `hevc` | H.265 + G.711 μ-law | fMP4, 음성 AAC 변환 | 2초 |

MinIO 버킷은 비공개다. MinIO와 RTSP 포트는 호스트에 공개하지 않고,
운영자 API만 `127.0.0.1:8090`에 바인딩한다. MinIO와 MediaMTX는 이 Compose의
개발용 저장소·카메라 대역이다. 운영 구성은 실제 카메라와 비공개 S3를 사용한다.

포트를 바꿀 때는 `.env.solution`의 `TRANSMUX_HTTP_PORT`와
`TRANSMUX_PUBLIC_URL`을 함께 바꾼다. `TRANSMUX_PUBLIC_URL`은 재생 설정의
`public_url`을 덮어쓴다. 공개 URL은 프록시 외부에서 접근하는 정확한 origin이어야 한다.

## 설정

수집은 `configs/solution-ingest.json`, 재생은 `configs/solution-playback.json`으로
설정한다. 다음 항목은 재생 서버(`playbackd`)의 설정이다.

| 키 | 기본값 | 의미·제한 |
|---|---|---|
| `auth.trusted_proxies` | `[]` | `X-Forwarded-For`를 신뢰할 프록시의 CIDR 문자열 배열. 최대 64개 |
| `export.min_free_bytes` | `1073741824` (1GiB) | 내보내기 볼륨에 남겨 둘 최소 여유 바이트. 허용 범위 128MiB∼1TiB |

TLS 리버스 프록시 뒤에 배포할 때는 `auth.trusted_proxies`에 실제 프록시의 CIDR을
반드시 지정한다. 직접 연결한 `RemoteAddr`가 신뢰 범위에 속할 때만
`X-Forwarded-For`를 오른쪽부터 읽고, 신뢰 프록시가 아닌 첫 주소를 클라이언트로
판정한다. 배열이 비어 있거나 직접 연결한 주소가 신뢰 범위 밖이면 헤더를 무시하고
`RemoteAddr`만 사용한다. 상세 배포 조건은 [배포 보안 가이드](deployment-security.md)를 따른다.

내보내기는 작업별 입력 크기의 3배를 기준으로 공간을 예약한다. 다른 작업의 예약과
`export.min_free_bytes`를 고려해 공간이 부족하면 작업 시작 전에
`507 export_storage_full`로 거절한다. 인덱스와 내보내기가 같은 볼륨을 사용하면
인덱스 갱신에 필요한 여유도 이 값에 반영한다.

## 데이터 흐름

```mermaid
flowchart LR
  C[RTSP 카메라] --> I[transmuxd<br/>채널 소유권·ffmpeg]
  I --> S[(비공개 S3<br/>세그먼트·매니페스트·카메라 목록)]
  S --> P[playbackd<br/>권한 검사·HLS 전달]
  S --> X[독립 백그라운드 인덱서]
  X --> D[(영속 시간 인덱스)]
  D --> P
  P --> U[운영자 화면<br/>라이브·녹화·관리]
  D --> E[제한된 ffmpeg 내보내기 작업]
  S --> E
  E --> U
```

- 세그먼트와 초기화 객체를 먼저 저장하고 매니페스트를 발행한다.
- fMP4 초기화 파일은 내용의 SHA-256으로 이름을 만들고 캡처 날짜마다 저장한다.
  전날 객체를 정리해도 다음 날의 긴 RTSP 세션이 끊어지지 않는다.
- 수집의 성공 경로는 인덱스 DB에 의존하지 않는다.
- 인덱서는 최근 라이브 매니페스트를 별도 작업 풀에서 추적한다.
  전체 복구가 오래 걸려도 새 녹화 인덱싱을 기다리게 하지 않는다.
- S3 목록은 백그라운드에서 256개씩 읽고 커서를 저장한다. 최근 꼬리 구간을
  겹쳐 확인하고 매일 전체 재검사를 시작해 커서 앞에 늦게 도착한 객체도 복구한다.
- 조회는 bbolt의 카메라·UTC 날짜 파티션과 캡처 시각·시퀀스 키를 검색한다.
  시청자의 조회 요청마다 S3 전체 목록이나 모든 세그먼트의 HEAD를 실행하지 않는다.

## 인증과 재생

`auth.users`는 자체 운영 계정의 최소 구성이다. 비밀번호는 지정한 환경변수에서
읽어 메모리에서 salt와 PBKDF2-SHA256 600,000회로 처리한다. 로그인 시도와
동시 비밀번호 검증 수를 제한한다.

로그인 한도는 설정 키가 없는 상수이며 1분 고정 창으로 집계한다.
설정된 실제 계정에는 **실패한 시도만 계정별 10회/분**으로 적용하고, 성공한
로그인은 계정 한도에 포함하지 않는다. 한도를 소진하면 해당 창이 끝날 때까지
최대 1분 동안 그 계정의 로그인은 올바른 비밀번호를 보내도 `429 login_rate_limit`로
거절한다. 이는 의도된 계정 잠금 정책이다.
클라이언트별로는 모든 로그인 시도에 **30회/분**을 적용하며 IPv6 주소는 `/64`
단위로 묶는다. 미존재 계정 이름은 계정별 집계에 넣지 않고 클라이언트 한도만 적용한다.

기존 인증 시스템은 신뢰할 수 있는 중앙 백엔드에서 아래 JWT 계약을 발급해
연동할 수 있다. 서명 키는 브라우저나 개별 테넌트에 배포하지 않는다.
기존 시스템의 RS256/JWKS 토큰을 직접 검증하는 어댑터는 포함되어 있지 않다.

```json
{
  "iss": "transmux",
  "aud": "transmux-api",
  "sub": "operator-123",
  "iat": 1789171200,
  "exp": 1789200000,
  "token_type": "access",
  "grants": [
    {
      "center_id": "center-01",
      "camera_ids": ["cam-01", "cam-02"],
      "permissions": ["live", "recording", "export"]
    }
  ]
}
```

- 알고리즘은 HS256으로 고정하고 issuer, audience, 발급·만료 시각, 토큰 종류를 검증한다.
- 각 grant는 센터·카메라 범위와 `live`, `recording`, `export`, `manage` 권한을 지정한다.
  `*`는 명시적 전체 범위다.
- 재생 세션은 API 토큰과 다른 audience를 가진 짧은 capability를 발급한다.
  상대 세그먼트 URL과 네이티브 HLS 플레이어가 동일한 권한을 이어받도록
  `/media/{capability}/{center}/{camera}/index.m3u8` 경로를 사용한다.
- 매니페스트, 영상 조각, 초기화 파일, HEAD·Range 요청마다 인증한다.
  Range 요청으로 만료를 우회할 수 없다. 제어 객체는 재생 경로에서 허용하지 않는다.
- 매니페스트는 `no-store`, 영상도 `private, no-store`로 전달한다.
  현재 구성은 인증 프록시이며 CloudFront 공유 캐시·서명 쿠키 구현은 아니다.
- origin은 정확히 허용 목록과 비교한다. API는 쿠키 대신 Bearer 토큰을 사용한다.
- 운영자 화면은 토큰을 JS 메모리에만 보관한다. 새로고침 시 다시 로그인한다.
  명시적으로 발급한 URL은 만료 전까지 capability다. 프록시에서 이 경로를
  액세스 로그·분석 도구로 수집하지 않아야 한다.

기본 API 토큰은 8시간, 재생 세션은 15분이다. 운영자 화면은 세션 만료 전에
재발급하고 플레이어를 다시 연결한다. 녹화 재발급은 같은 고정 목록을 유지한다.
권한 회수는 유효한 기존 토큰의 만료 또는 서명 키 교체 시 반영된다.
즉시 개별 토큰 폐기 목록은 구현하지 않았다.

## API

JSON 요청은 `Content-Type: application/json`, 관리·조회 요청은
`Authorization: Bearer {access_token}`을 사용한다. 오류는
`{"error":{"code":"...","message":"..."}}` 형식이다.

| 메서드·경로 | 동작 |
|---|---|
| `POST /v1/login` | `username`, `password`로 API 토큰 발급. 로그인 한도 초과·계정 잠금 시 `429 login_rate_limit` |
| `GET /v1/me` | 사용자, 범위, 만료 시각, 관리 가능 여부 |
| `GET /v1/centers` | 권한이 있는 센터와 카메라 수 |
| `GET /v1/cameras` | 권한이 있는 카메라 목록 |
| `GET /v1/centers/{center}/cameras` | 센터별 목록 |
| `POST /v1/playback-sessions` | 1∼16개 카메라의 라이브·녹화 세션 |
| `POST /v1/playback-sessions/renew` | `session_token`으로 기존 세션 갱신 |
| `GET /v1/recordings` | 카메라·시각 구간의 녹화 기간과 인덱스 복구 상태 |
| `POST /v1/exports` | 사전 검사와 공간 예약 성공 시 MP4 준비 작업 시작, `202`와 작업 ID 반환 |
| `GET /v1/exports` | 해당 사용자의 내보내기 작업 |
| `GET /v1/exports/{id}` | 상태·실패 코드·준비된 다운로드 URL |
| `DELETE /v1/exports/{id}` | 작업 취소 또는 준비된 파일 제거 |
| `GET /v1/admin/cameras` | 편집용 목록, ETag, 담당 서버와 한도 |
| `POST /v1/admin/cameras` | 카메라 추가 |
| `PUT /v1/admin/cameras/{center}/{camera}` | 카메라 설정 교체 |
| `DELETE /v1/admin/cameras/{center}/{camera}` | 수집 중지, 녹화와 목록 유지 |
| `GET /v1/admin/status` | 인덱서·보존 작업 상태, 동시 요청 수 |
| `GET /v1/admin/retention` | 보존 정책과 삭제 후보 표본, 읽기 전용 |
| `GET /media/{capability}/...` | 인증된 HLS 매니페스트·미디어 |
| `GET /exports/{capability}/clip.mp4` | 완료된 파일 다운로드, Range 지원 |
| `GET /healthz`, `GET /readyz` | 일반적인 생존·카탈로그 준비 상태 |

`POST /v1/exports`는 다음 오류를 작업 시작 전에 즉시 응답한다.

| HTTP 상태 | 오류 코드 | 의미 |
|---|---|---|
| `404` | `recording_not_found` | 요청 구간에 색인된 녹화가 없음 |
| `422` | `export_too_large` | 내보내기 크기·세그먼트 수 한도 초과 |
| `409` | `recording_format_changed` | 요청 구간이 MPEG-TS/fMP4 형식 변경을 가로지름 |
| `507` | `export_storage_full` | 예약 공간과 최소 여유를 확보할 수 없음. `Retry-After: 60` 응답 |

라이브 세션 요청:

```json
{
  "center_id": "center-01",
  "camera_ids": ["cam-01"],
  "mode": "live"
}
```

녹화는 `mode: "recording"`에 `start`, `end` RFC3339 시각을 추가한다.
응답의 `streams`에는 `camera_id`, `url`, `expires_at`이 있고,
녹화에는 실제 조각 경계인 `actual_start`, `actual_end`, `segments`도 있다.

녹화 목록의 쿼리 파라미터와 내보내기 JSON은 다음 필드를 사용한다.

```json
{
  "center_id": "center-01",
  "camera_id": "cam-01",
  "start": "2026-09-12T00:00:00Z",
  "end": "2026-09-12T00:10:00Z"
}
```

조회는 최대 24시간, 재생은 최대 6시간·12,000조각이다.
내보내기의 기본 한도는 2시간·4GiB이며 설정으로 조절한다.
`index_complete: false`이면 과거 S3 목록을 아직 모두 복구하지 못한 상태다.
조회 결과가 없다는 응답과 전체 목록 복구 완료는 구분해야 한다.

녹화 시작·종료는 조각 경계에 맞춘다. 자정에 걸친 구간을 함께 조회하고,
연결 단절은 `EXT-X-DISCONTINUITY`, 종료는 `EXT-X-ENDLIST`로 표현한다.
세션 생성 시 매니페스트와 허용 객체 목록을 영속 저장하므로 늦은 인덱싱이
이미 재생 중인 VOD를 변경하지 않는다. 보존 기간이 끝나 실제 객체가 지워지면
기존 세션도 해당 영상을 더 이상 전달할 수 없다.

인덱스는 S3 목록과 주기적으로 대조된다. 각 날짜는 하루 한 번 처음부터 다시
나열되며, 목록 시작 전에 인덱싱됐는데 목록에 없는 조각은 S3 Lifecycle 등으로
삭제된 것으로 보고 인덱스에서 제거한다. 따라서 외부에서 지운 녹화는 다음 일일
대조(최대 약 24시간 뒤)가 그 구간을 지날 때 `/v1/recordings`와 새 세션 발급
대상에서 빠진다. 대조 전에는 세션이
발급되더라도 해당 조각 요청이 404로 끝날 수 있다. 인덱스 창은 현재 시각부터
`index.days`일 전까지다. 종료 시각이 그 경계 이후인 조각은 지우지 않는다. 날짜
파티션은 그날의 모든 조각이 경계 전에 끝났음이 보장될 때, 즉 다음 날 00:00Z에
최대 조각 길이 1시간을 더한 시각이 경계 이전일 때만 통째로 정리한다. 스캐너는
이 창에 남는 모든 날짜를 대조한다. 정리는 전체 스캔과 별도 주기(`poll_interval`)로
짧은 트랜잭션을 반복하므로 스캔이 느려도 만료 날짜가 쌓이지 않는다. 라이브
매니페스트에 남은 창 밖 조각도 다시 인덱싱하지 않는다. 창보다 오래된 녹화는 S3에
남아 있어도 조회되지 않으므로 `index.days`는 보존 기간 이상으로 설정한다.
이전 버전의 인덱스 파일은 대조 시 날짜별로 1,000건씩 나눠 변환한다.
느린 저장소에서 한 페이지를 제한 시간 안에 다 확인하지 못해도 확인한 객체까지
반영하고 이어서 진행한다.

MPEG-TS와 fMP4 형식 변경을 가로지르는 VOD는 구간을 나눠 요청해야 한다.
현재는 프레임 단위 트리밍·재인코딩을 수행하지 않는다.

## 카메라 관리와 샤딩

수집 설정에서 `cameras.provider: "object"`와
`cameras.object_key: "_transmux/rosters/{name}.json"`을 사용한다.
이 값은 버킷 루트 기준의 전체 키다. 미디어의 `storage.key_prefix`를 붙이지 않는다.
`cameras.static`은 객체가 없을 때 한 번만 적용되는 초기 목록이다.
이후 JSON 파일의 초기 목록을 바꿔도 저장된 목록을 덮어쓰지 않는다.

```json
{
  "center_id": "center-01",
  "camera_id": "cam-01",
  "shard_id": "shard-01",
  "name": "정문",
  "rtsp_url": "rtsp://camera-user:password@camera-address/stream",
  "enabled": true,
  "video_codec": "h264",
  "format": "fmp4",
  "audio": "aac"
}
```

H.265 카메라는 `video_codec: "hevc"`와 `format: "fmp4"`를 함께 지정한다.
영상은 복사하면서 Apple 재생에 필요한 MP4 sample entry `hvc1` 표시를 적용한다.
코덱을 생략하거나 `auto`로 지정하면 ffmpeg의 원본 기본 설정을 유지하며,
HEVC의 `hvc1` 표시를 보장하지 않는다. 실제 카메라와 다른 코덱을 지정하면
ffmpeg가 호환되지 않는 입력을 거절할 수 있다.

카메라 변경에는 편집용 GET 응답의 ETag를 `If-Match`로 보낸다.
버전이 바뀌면 `412`, 없으면 `428`이며 무조건 덮어쓰지 않는다.
RTSP 주소를 비워서 PUT하면 기존 비밀 주소를 유지한다. 조회 응답에는
주소를 복원할 수 있는 비밀번호·쿼리 문자열을 반환하지 않는다.
`DELETE`는 영상 수집을 끄며, 과거 녹화를 찾을 수 있도록 목록을 유지한다.

여러 수집 서버는 같은 목록을 읽되 각 설정의 `cameras.shard_filter`를
다르게 지정한다. 목록의 `shard_id`가 일치하는 카메라만 수집한다.
재생 서버는 전체 목록을 읽으므로 센터·카메라를 한 화면에서 조회한다.
담당 서버를 바꾸면 기존 서버가 연결을 종료하고 새 서버가 소유권을 획득한다.
같은 `shard_filter`의 대기 복제본은 기존 lease 프로토콜로 중복 수집을 막는다.

재생 설정의 `shards`에 담당 서버별 한도를 등록한다.

```json
{
  "shards": [
    {"id": "shard-01", "max_channels": 75},
    {"id": "shard-02", "max_channels": 75}
  ]
}
```

각 수집 설정의 `max_channels`도 해당 한도에 맞춘다. 한도를 넘는 활성화는
관리 API가 거절한다. 이 설정은 용량 보장이 아니며 실제 장비에서 측정한
한도로 설정해야 한다. 기존 목록을 샤드 필터로 옮길 때는 모든 카메라에
`shard_id`를 먼저 배정해야 한다.

기존 `static`, `http` 공급자는 계속 지원한다. 이 경우 자체 관리 API는
카메라 변경을 거절하고 외부 공급자를 시스템 원장으로 사용한다.
HTTP 공급자의 `enabled: false` 카메라는 수집에서 제외하지만 재생 카탈로그와
보존 작업 대상에는 유지하므로 권한이 있는 사용자는 과거 녹화를 계속 조회·재생할 수 있다.
수집 설정에 `cameras.shard_filter`를 지정한 경우 HTTP 카탈로그에도 적용하므로
다른 샤드의 카메라는 해당 재생 서버의 카탈로그·보존 작업에 포함하지 않는다.

## 보존과 내보내기

보존 삭제의 기본값은 꺼짐이다. 운영 설정의 `retention.enabled`로 켠다.
후보 미리 보기는 읽기 전용이며 API로 우발적으로 전체 삭제를 실행하지 않는다.

정리 작업은 현재 카메라 목록의 S3 경로를 직접 순회하므로 인덱스를 잃어버린
상태에서도 적용할 수 있다. 자신의 세그먼트·초기화 파일 명명 규칙과 메타데이터를
모두 검사한다. 라이브 매니페스트, lease, 카메라 목록과 다른 파일은 제외한다.
UTC 전체 날짜와 최대 조각 길이를 보수적으로 계산하므로 설정 기간보다
약 1∼2일 더 보존할 수 있다. 카메라별 커서는 디스크에 저장한다.
카메라마다 한 차례에 최대 64개씩 처리해 한 카메라가 다른 카메라의 정리를
계속 밀어내지 않게 하고, 배치가 남으면 만료 객체가 남은 카메라를 다시 돌며
배치를 소진한다.

삭제 처리율은 `batch / interval`이다. 배치는 검사한 객체 수로 계산하므로
만료되지 않은 첫 객체 확인과 메타데이터가 맞지 않아 건너뛴 객체도 포함한다. 예를 들어 625대가 4초마다 저장하면
하루 약 1,350만 조각이므로 기본 1,000개/시간 설정으로는 따라갈 수 없다.
운영에서는 처리율에 맞게 작업 주기·배치를 정하거나 S3 Lifecycle을 사용해야 한다.
대규모 저장에는 [태그 기반 Lifecycle 예시](../deploy/s3-lifecycle.example.json)를
제공한다. 수집 설정의 `storage.tag_media: true`와 `s3:PutObjectTagging` 권한을
함께 적용하면 영상 조각에는 `transmux-kind=segment`, 초기화 파일에는
`transmux-kind=init` 태그가 붙는다. 매니페스트·lease·공유 목록에는 붙이지 않는다.
예시는 조각을 30일, 초기화 객체를 32일 보존해 같은 날짜의 늦은 조각도 재생할 수
있게 한다. 기본 `tag_media`는 기존 IAM 구성과 호환되도록 꺼져 있고 모의 스택은 켠다.
Lifecycle 예시의 접두사·기간은 실제 구성에 맞추고, 기존의 태그 없는 객체는
백그라운드 정리 경로 또는 별도의 이관 작업으로 처리한다.
버킷 버전 관리가 켜져 있으면 DeleteObject는 삭제 마커를 만들므로
비현재 버전의 실제 공간 회수는 별도 Lifecycle 정책이 필요하다.

내보내기는 다음 한도를 갖는다.

- 기본 동시 작업 2개, 처리 시간 10분, 준비된 작업 포함 최대 20개.
- 입력·출력·MP4 faststart에 필요한 여유 공간 확인.
- 입력을 한꺼번에 메모리에 읽지 않고 제한된 버퍼로 임시 파일에 복사.
- ffmpeg에는 직접 만든 로컬 매니페스트만 전달하고 `file` 프로토콜만 허용.
- 취소·시간 초과·종료 시 프로세스 그룹 종료 및 임시 파일 정리.
- 완료 파일은 1시간 보관하고 브라우저는 일반 GET 다운로드로 저장.

작업 목록은 재생 프로세스 메모리에 있다. 브라우저 새로고침 후에는 목록을
재조회할 수 있지만, 프로세스 재시작 후에는 작업을 다시 요청해야 한다.
재시작 시 잠긴 인덱스의 고유 ID에 속하는 미완료 임시 디렉터리를 정리한다.

## 운영 배치

`configs/solution-*.json`과 개발 Compose를 그대로 인터넷에 공개하는 구성은
운영 배치가 아니다. 운영용 파일은 다음 실제 환경 값으로 만든다.

1. 비공개 S3 버킷, 실제 RTSP 주소, 캡처 환경의 GOP·시계 동기화.
2. 수집 역할과 재생·인덱스 역할의 분리된 IAM 권한.
3. 재생 origin의 HTTPS 종단, 인증 연동 또는 운영 계정·secret 주입.
4. 인덱스·MP4 임시 파일용 영속 디스크와 용량·처리율 감시.
5. 실측 서버별 채널 수, 동시에 보는 화면 수, 목표 보존량·전송량.

수집 역할은 채널의 Get/PutObject와 공유 목록의 읽기·최초 생성 권한이 필요하다.
재생 역할은 미디어 GetObject, 카메라 목록 Get/PutObject, 인덱싱용 ListBucket이
필요하며 보존 삭제를 켤 때만 날짜별 미디어의 DeleteObject를 추가한다.
lease·매니페스트·공유 목록에 DeleteObject를 허용할 이유는 없다.

현재 재생 서비스의 기준 배치는 **단일 playbackd와 영속 인덱스 파일**이다.
한 인덱스 파일은 한 프로세스만 열 수 있다. 여러 재생 복제본을 쓸 때는 각각
독립 인덱스와 고정 라우팅이 필요하다. 녹화 세션과 내보내기 상태를 공유하는
외부 DB, 완전한 재생 서버 장애 전환은 아직 제공하지 않는다.

2∼8초 GOP 기반 일반 HLS이므로 초저지연 서비스가 아니다. HEVC fMP4의
브라우저 재생은 클라이언트의 HEVC 디코더 지원에 달려 있다.
625채널 예시의 8GB 메모리·30일 연속 운영은 이 기능 구현만으로 인증되지 않는다.
측정과 실제 전환 조건은 [용량 모델](capacity-model.md),
[기존 미디어 서버에서의 전환](wowza-coverage.md)을 함께 적용한다.

## 검증 도구

```sh
make fmt-check vet test race build vuln
make test-ffmpeg
make solution-verify
```

`solution-verify`는 비공개 버킷의 익명 접근 거절, 범위 권한, HTTP Range,
미디어·제어 객체의 태그 분리와 세 코덱 조합의 라이브·녹화·MP4를 실제로 디코딩한다.
`python3 scripts/verify-solution.py --restart`는 모의 수집·재생 컨테이너도 재생성해
기존 VOD 세션·Range 요청이 유지되고 새 수집 시퀀스가 전진하는지 확인한다.

운영자 화면 검증에는 Node.js 24 LTS를 권장한다.

```sh
cd tests/browser
npm ci
npx playwright install chromium
npm test
```

브라우저 검증은 실제 H.264 TS·fMP4 재생, 녹화와 네이티브 파일 다운로드,
카메라 추가 후 실제 수집·중지, 계정 권한, 중복 제출, 오류 복구,
좁은 화면·키보드·axe 접근성 검사를 포함한다.
테스트 데이터는 모의 스택에만 작성한다. HEVC의 모든 브라우저·네이티브
앱 조합, 보조기기 수동 검사, 실제 운영 카메라 호환성을 대신하지 않는다.
