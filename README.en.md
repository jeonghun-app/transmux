# transmux — IP camera live and recorded video solution

[![CI](https://github.com/jeonghun-app/transmux/actions/workflows/ci.yml/badge.svg)](https://github.com/jeonghun-app/transmux/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
[![Go 1.26.8](https://img.shields.io/badge/Go-1.26.8-00ADD8.svg)](go.mod)

[한국어](README.md)

**Provides live monitoring and recorded video search and playback for IP cameras.**
`transmuxd` saves RTSP video as HLS, while `playbackd` provides authenticated live
and recorded playback, camera management, MP4 export, and an operator interface.

It supports H.264/MPEG-TS and H.264 or H.265/fMP4. Video is copied without
re-encoding (`-c:v copy`). For each camera, audio can be omitted, copied, or
transcoded to AAC. Video in private S3 is delivered through a playback server
that checks center and camera permissions.

```
IP camera ──RTSP/TCP──> transmuxd ──> private S3
                           │               │
                    ownership/reconnect    ├──> recording index
                                           │         │
                                           └──> playbackd ──> operator UI/clients
                                                auth/HLS/MP4
```

A Go supervisor manages one ffmpeg process per channel. See
[docs/adr/0001-architecture.md](docs/adr/0001-architecture.md) for the rationale
and alternatives, and [docs/capacity-model.md](docs/capacity-model.md) for measured
capacity.

The [solution guide](docs/solution.md) documents the runtime, API, and deployment
contracts. [Migration from an existing media server](docs/wowza-coverage.md)
describes feature coverage and migration conditions, and
[ADR 0003](docs/adr/0003-camera-solution.md) records the current delivery
architecture. This is not a product that offers every broadcast server feature,
such as WebRTC, RTMP, SRT, and ABR.

## Project status and supported platforms

This is currently a **pre-1.0** project. APIs, configuration, and storage formats
may change in minor releases. Pin versions and review changes and verification
coverage before upgrading. See the [security policy](SECURITY.md) for the scope
of security fix support and reporting instructions.

| Platform | Support scope |
|---|---|
| Linux/amd64 | Reference environment for ingest and playback services and container-based verification |
| macOS and Windows | Development through Linux containers on Docker Desktop. Native service execution has not been verified |
| Other CPU architectures | Require separate verification of builds, ffmpeg, and actual media paths |

Browser playback targets H.264; HEVC depends on decoder support in the OS and
browser. Before production deployment, verify with real cameras, client devices,
and expected load.

## Quick start

Docker/Compose v2 and Python 3 are required. Go and ffmpeg do not need to be
installed on the host. Builds and checks use Go 1.26.8 / Alpine 3.23; the
runtime uses Alpine 3.23.

```bash
make solution-up      # Private storage + ingest/playback servers + 3 simulated cameras
make solution-verify  # Authentication, live/recorded playback, Range, actual MP4 decoding
make solution-down    # Stop; preserve recording and configuration volumes
```

The operator interface is at `http://localhost:8090`. Passwords for `admin` and
`viewer` are in the generated `.env.solution`. See the
[solution guide](docs/solution.md#로컬-실행) for accounts, port configuration, and
browser verification.

Development checks:

```bash
make help          # Available targets
make fmt-check     # Check Go formatting
make vet           # Static analysis
make test          # Unit tests
make race          # Concurrency checks
make build         # Build ingest and playback binaries
make vuln          # Check module integrity and reachable Go vulnerabilities
make test-ffmpeg   # Integration tests requiring real ffmpeg
make image         # Build runtime image
```

The earlier ingest-only PoC and capacity measurement setup remain available.

```bash
make poc-up           # 2 simulated cameras + MediaMTX + transmuxd + MinIO
make poc-up-capacity  # Above + 1080p/D1/720p sources for capacity measurements
make poc-verify       # End-to-end verification (playability, manifest consistency)
make poc-logs         # transmuxd logs
make poc-down         # Clean up
```

`make poc-up` runs the complete pipeline without real IP cameras or real S3.
MediaMTX acts as the cameras and MinIO as S3. The two simulated cameras have
different keyframe intervals. `cam-shortgop` has a 2-second GOP and can meet the
4-second target; `cam-longgop` has an 8-second GOP and cannot. Both must operate
normally.

Capacity measurements:

```bash
make poc-up-capacity                              # Include 1080p/D1/720p sources
./scripts/measure-capacity.sh 2 10 25 50          # Memory, FDs, process count
./scripts/measure-cpu.sh --profile hd 1 10 25 50  # CPU (precise cgroup measurement)
```

Instead of `docker stats`, `measure-cpu.sh` reads cgroup v2 `cpu.stat` to measure
actual CPU time consumed. It reports ffmpeg and supervisor separately and
regresses CPU against channel count to estimate marginal cost. Profiles are
`shortgop` (2-second GOP), `longgop` (8-second GOP), `d1` (704×480 0.5Mbps),
`hd720` (720p 1Mbps), and `hd` (1080p 4Mbps). `d1`, `hd720`, and `hd` require the
sources started by `poc-up-capacity`. Before measuring, the script probes the
actual stream and rejects a missing publisher: a path without a publisher is
simply silent, rather than an RTSP-layer error, and would produce idle-channel
CPU measurements.

## Configuration

Ingest and playback each use a JSON file (`configs/solution-ingest.json`,
`configs/solution-playback.json`). Unknown keys, invalid camera identifiers or
RTSP URLs, duplicate cameras, invalid HTTP endpoints, and unsafe object prefixes
are rejected before startup. The same validation applies to `-validate`.

```bash
transmuxd -config /etc/transmux/config.json -validate   # Validate configuration only
playbackd -config /etc/transmux/playback.json -validate
```

Key ingest settings (`configs/solution-ingest.json`):

| Key | Meaning | Notes |
|---|---|---|
| `http_listen` | Ingest server's internal monitoring address | Defaults to `127.0.0.1:8080`. Container examples specify `:8080` and restrict exposure on the host |
| `max_channels` | Maximum channels assigned to this shard | A ceiling that prevents load beyond verified capacity |
| `segment.target_duration` | `-hls_time` value | **A target and lower bound.** The camera GOP determines actual duration |
| `segment.live_window` | Number of segments retained in the published manifest | |
| `segment.local_list_size` | ffmpeg `-hls_list_size` | Number of segments left in the spool. Must exceed `live_window`; validation enforces this. The grace period is this value × **actual** segment duration (= camera GOP), so calculating it with target_duration overestimates the available time |
| `segment.max_gop_slack` | Extra allowance when detecting missing segments | Prevents long-GOP cameras from being mistaken for failures |
| `ffmpeg.stall_timeout` | Threshold for stopping ffmpeg when it produces no segments | Primary disconnection detector, independent of ffmpeg version |
| `ffmpeg.input_args` | Additional arguments before `-i` | Place for version-specific socket timeout options |
| `storage.endpoint` / `force_path_style` | For MinIO/LocalStack | Leave empty in production |
| `storage.key_prefix` | Prefix for media and lease objects | Leading and trailing `/` are normalized. `/tenant/video/` equals `tenant/video`; the prefix does not apply to shared camera roster keys |
| `upload.max_concurrent` | Concurrent segment and manifest storage requests | Ownership requests reserve another `min(16, max_concurrent)` slots so an upload backlog cannot prevent lease renewal |
| `storage.tag_media` | `transmux-kind` object tag for retention | Off by default. Enabling it requires `s3:PutObjectTagging`; control objects are not tagged |
| `cameras.provider` | `static`, `http`, `object` | `object` uses a conditionally updated shared roster and management API. Disabled `http` cameras are excluded from ingest but retained in the playback catalog and retention processing within the shard filter |
| `cameras.shard_filter` | Responsible ingest server ID | Ingest only cameras whose roster `shard_id` matches |
| `cameras.static[].format` | `mpegts` or `fmp4` | Defaults to `mpegts`; `fmp4` is recommended for HEVC |
| `cameras.static[].video_codec` | `auto`, `h264`, `hevc` | With `fmp4`, HEVC gets an Apple-compatible `hvc1` designation |
| `cameras.static[].audio` | `none`, `copy`, `aac` | Defaults to `none`; transcode G.711 and similar audio to `aac` |
| `lease.ttl` | Ownership expiration time | Takeover adds `2×max_clock_skew` clock-skew allowance, the next roster/lease check, and storage request time to the TTL |
| `lease.renew_interval` | Renewal interval | Enforces `ttl > 2×renew_interval + 2×operation_timeout + max_clock_skew`. Even after one missed renewal, the next write and lost-response check must finish before the owner's own deadline |
| `lease.operation_timeout` | Limit for one lease request | Separate from `upload.put_timeout`. Lease records are hundreds of bytes while segments are several MB, so one shared timeout would necessarily be wrong for one of them |
| `lease.max_clock_skew` | Assumed maximum clock skew between shards | The taker waits this much longer after expiry; the owner stops this much earlier. Hosts need NTP |

Key playback settings (`configs/solution-playback.json`):

| Key | Meaning | Notes |
|---|---|---|
| `auth.trusted_proxies` | Array of proxy CIDR strings trusted for `X-Forwarded-For` | Defaults to `[]`, with at most 64 entries. An empty array uses only `RemoteAddr`. Deployments behind a TLS reverse proxy must specify the actual proxy CIDRs |
| `export.min_free_bytes` | Minimum free bytes to retain on the export volume | Defaults to `1073741824` (1GiB), with an allowed range of 128MiB–1TiB. Each job reserves space based on three times its input size; insufficient space returns `507 export_storage_full` (`Retry-After: 60`) before the job starts |

Only failed attempts for configured accounts count toward the per-account limit
of 10 per minute. Successful logins do not count toward that limit. Once it is
exhausted, the account is locked for the remainder of its fixed window (up to
one minute), and even the correct password receives `429`.
The client limit covers all login attempts at 30 per minute, grouping IPv6
addresses by `/64`. Nonexistent account names are subject only to the client
limit. These limits are constants with no configuration keys. See the
[solution API guide](docs/solution.md#api) for immediate export error responses.

Credentials are read through the AWS SDK default chain. ECS task roles or EKS
IRSA are recommended in production. MinIO development uses
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`. When using temporary environment
credentials, pass `AWS_SESSION_TOKEN` as well.

Required S3 permissions for the ingest prefix are `s3:PutObject` and
`s3:GetObject`. Read permission includes HEAD checks for manifest, lease, and
segment conflicts. To distinguish GET of a nonexistent key as `404`,
`s3:ListBucket` permission on the bucket is also required. Without it, S3 may
return `403` even for a missing key; the daemon waits for ownership confirmation
instead of guessing that a camera is new. `transmuxd` does not call object
listing or deletion APIs. `playbackd` needs listing permission for indexing and,
if retention deletion is enabled, permission to delete media. See the
[deployment security guide](docs/deployment-security.md) for permissions by role.

A persistent volume is **not required** for the ingest `state_dir` — at each
startup, the published manifest is read to establish the sequence, while the
local checkpoint serves only as a floor below which the sequence must never
fall. The checkpoint is written **after** a successful manifest PUT. If the
process dies between them, it can lag behind the manifest and therefore cannot
be authoritative on its own. The larger of the two values is used. Use separate
persistent storage for the playback recording index and pinned VOD lists.

## Object layout

```
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/seg-{sequence}-{unixms}.ts
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/seg-{sequence}-{unixms}.m4s
s3://bucket/{prefix}/{center_id}/{camera_id}/{YYYY}/{MM}/{DD}/init-{sha256}.mp4
s3://bucket/{prefix}/{center_id}/{camera_id}/index.m3u8
s3://bucket/{prefix}/{center_id}/{camera_id}/_transmux/lease.json
s3://bucket/_transmux/rosters/{name}.json
```

Segments carry S3 user metadata (`sequence`, `pdt-ms`, `duration-ms`,
`discontinuity`, `center-id`, `camera-id`). Actual duration cannot be recovered
from the object key, and estimating it from the next segment timestamp fails
across a camera disconnection, so it is recorded at upload time. fMP4 also
records `init-uri`. A background indexer in `playbackd` validates this metadata
to build and recover the time index.

Segments live in date directories, while the live manifest is kept at the
camera root. Putting the manifest in a date directory would change the playback
URL at every midnight. Its name can be changed with `storage.manifest_name`.

The date is based on the segment's own wall-clock time, not upload time. A
segment captured just before midnight does not move to the next date because
its upload was delayed.

## Ingest server monitoring

| Endpoint | Purpose |
|---|---|
| `GET /livez` | Process liveness only; unaffected by camera or S3 failures |
| `GET /readyz` | Roster loaded + at least half the channels healthy (both segment storage and manifest publication must be recent) |
| `GET /healthz` | Aggregate for Zabbix/WhaTap. Always returns 200; alert from body fields |
| `GET /channels` | Full channel inventory |
| `GET /channels/{center_id}/{camera_id}` | Individual channel |
| `GET /metrics` | Prometheus text format |

Channel states (`transmux_channel_state`): 1 starting, 2 receiving,
3 disconnected, 4 reconnecting, 5 stopping, 6 stopped, **7 failed**,
8 waiting_ownership.

`failed` does not recover automatically and requires operator intervention. A
channel enters this state when the manifest is not ours or another shard takes
its ownership.

`waiting_ownership` is a **normal waiting state**. Another shard may still own
this camera (normal during a rolling deployment), or the object store may be
unreadable, preventing ownership confirmation. The channel publishes nothing
and does not start ffmpeg; it recovers on its own when the condition clears.
This is distinct from `failed` because a temporary store outage should not
force operator intervention on all 75 channels.

## Multi-host ownership

Exactly one shard must receive each camera. If two shards write the same
manifest, the playlist switches between two timelines. Segments have a
one-year immutable cache, so reusing a key causes **CDN cache contamination**,
not a safe overwrite.

This is resolved using only conditional writes to the object store (no
separate database).

1. **Lease** — Create `{prefix}/{center_id}/{camera_id}/_transmux/lease.json`
   with `If-None-Match: *` and renew it with `If-Match`. If another shard holds
   a live lease, do not start ffmpeg.
2. **Fence** — A lease alone is insufficient: a frozen owner can wake up and
   still use a valid CAS token. Therefore, **before starting ffmpeg**, the new
   owner rewrites the manifest conditional on the version it just read. The
   body includes a different `# transmux-write-id` comment on every write, so
   the ETag changes even if the playlist content is otherwise identical. The
   previous owner's token is invalidated at that instant. Ownership transfers
   at this fence write, not when the lease is acquired.
3. **Create-only segments** — Use `If-None-Match: *`. If a key already exists,
   check with `HEAD` whether it was our own retry (matching `put-id`) or
   somebody else's object. For another writer's object, stop the channel
   without overwriting it.

The upload layer does **not automatically retry** conditional writes. If a
write lands but only its response is lost, a retry sees its own precondition
already consumed and receives 412, indistinguishable from losing to another
writer. Only the caller knows which write ID it used, so the caller reads back
and verifies. If that verification request also fails, it retains the original
write ID and body and reads again first on the next attempt. It retries with
the same ID only if the stored version is unchanged. Once it confirms its own
write, it adopts that ETag and publishes the latest manifest.

Sometimes the fence writes an empty window (when the checkpoint floor is ahead
of the manifest, the window is discarded). The manifest body therefore records
the highest sequence published so far in `# transmux-last-sequence`. Without
this, the fence would publish `EXT-X-MEDIA-SEQUENCE:0`; a process later
recovering from that manifest alone would restart at sequence 0 and reuse
still-live object keys.

`# transmux-write-id` and `# transmux-last-sequence` are **format markers, not
authentication**. Anyone who can write to the bucket can also forge a manifest
that references segments. The trust boundary is IAM and CAS. These comments
change the ETag and identify our own writes.

Each channel adds one small PUT every `lease.renew_interval`. At 625 channels
and a 5-second interval, that is about 125 PUT/s in addition to 250 segment and
manifest PUT/s.

Deployment caution: older binaries that do not understand leases write without
conditions. This mechanism cannot make a mixed old/new deployment safe. Stop
the old writers first. S3 lifecycle rules **must exclude** `_transmux/` and
live manifests.

It is intentional that `/livez` does not reflect camera or S3 status. If a
single camera or temporary S3 failure makes liveness fail, the orchestrator
restarts the container and interrupts all other channels on that shard,
turning a partial failure into a full-shard failure.

**Security: these endpoints have no authentication.** They expose camera
inventory and channel status (but not credentials or RTSP URLs containing
userinfo). Bind them to an internal interface or place them behind a security
group or service mesh. Do not expose them to the public internet or camera
network.

### Alert design

| Condition | Signal |
|---|---|
| Camera disconnection | `transmux_channel_state` = 3 or 4; `transmux_channel_reconnects_total` increases |
| Missing segment | `transmux_channel_segment_gap_alarm` = 1 |
| Lost segment (reclaimed before upload) | `transmux_channel_segments_lost_total` increases |
| S3 failure | `transmux_channel_segment_upload_failures_total` increases + `state` = 2(receiving) |
| Segment stored but absent from playback | `transmux_channel_manifest_upload_failures_total` increases; `seconds_since_manifest` rises |
| Spool near exhaustion | `transmux_channel_spool_oldest_seconds` approaches `local_list_size × segment duration` |
| Channel permanently stopped | `transmux_channel_failed_total` increases (operator intervention required) |
| Two shards receive one camera | Investigate immediately if `transmux_channel_manifest_conflicts_total` or `transmux_channel_segment_conflicts_total` is nonzero |
| Ownership transfer | `transmux_channel_lease_lost_total` increases; `transmux_channel_lease_held` = 1 on the new shard |
| Normal rolling deployment | `transmux_channel_lease_contended_total` increases briefly, then stops |
| Final flush failure on shutdown | `transmux_channel_final_drain_failures_total` increases |
| Camera provides no PDT | `transmux_channel_missing_pdt_total` increases (date directory falls back to upload time) |
| Distinguish camera from S3 failure | `last_ffmpeg_segment_timestamp` vs `last_segment_timestamp` |

The last comparison matters: if ffmpeg keeps producing segments while only the
publication timestamp stops, the object store is failing. If both stop, the
camera is failing.

`segments_published_total` rises on **successful storage of a segment object**.
For the player to see that segment, the manifest must also be updated; its
timestamp is reported by `transmux_channel_last_manifest_timestamp_seconds`.
Segment PUTs can succeed while manifest PUTs fail, so these signals must be
examined separately. `upload_failures_total` remains the sum of both failures.

`seconds_since_segment`, `seconds_since_manifest`, and `segment_gap_alarm` are
calculated at scrape time. If stored as gauges, they stop updating during
reconnect backoff or in the `failed` state, leaving stale values when they are
most needed. Even if the first media manifest has never succeeded,
`seconds_since_manifest` increases from the time the first segment was stored.
A channel that stores only segments without publishing a manifest is not
reported healthy.

The HTTP camera roster follows the `StaticCamera` JSON array contract. Unknown
fields, responses over 8MiB, responses with another document appended, and
`null` are errors; existing channels are retained. To release all cameras,
return an explicit `[]`.

## Known limitations and unimplemented features

- **Large-scale sustained load has not been verified.** Extrapolated memory for
  625 channels on the existing video-copy path is about 8.6GiB, exceeding, for
  example, an 8GB memory budget. Audio transcoding, indexing, viewing, and
  export loads need separate measurement. See the
  [capacity model](docs/capacity-model.md).
- **Standard HLS latency.** WebRTC, LL-HLS, and 1–3-second low-latency delivery
  are not provided.
- **HEVC client constraints.** Real fMP4 ingest, playback, and MP4 decoding
  have been verified, but browser playback depends on HEVC decoder support in
  the OS and browser.
- **Single playback server baseline.** The persistent bbolt index is opened by
  only one process. Sessions and export state are not shared between playback
  servers, and automatic failover is unavailable.
- **CDN deployment is separate.** Delivery currently uses an authenticated
  proxy. Configure CloudFront OAC and signed cookies, TLS termination, and
  external SSO integration for the operating environment.
- **Recording follows segment boundaries.** Frame-accurate cuts and a single
  VOD spanning a TS/fMP4 format change are unsupported.
- **No profile dimension in object paths.** Decide how to represent substreams
  (dual streams) for multi-view before URLs become an external contract. See
  ADR 0002 §8.
- **The HTTP provider supports only a fixed JSON array contract.** DBs or APIs
  using pagination or a separate response envelope need an adapter.
- **Retention deletion is off by default.** Background cleanup and an example
  tag-based S3 Lifecycle policy are provided. Set the production retention
  period, throughput, and versioning policy.

## Verified behavior

These items were verified in the PoC stack. Ingest failure tests are recorded
in the [quality review](docs/verification/2026-09-12-quality-review.md), and
playback, management, and export tests in the
[solution review](docs/verification/2026-09-12-solution-review.md).

- Authenticated H.264 TS, H.264 fMP4, and H.265 fMP4 live and recorded playback
  from a private bucket
- AAC copy and G.711→AAC transcoding, HTTP Range, and MP4 export that actually
  decodes
- Center and camera scope, management and export permissions, token expiry,
  algorithm, and path restrictions
- Pinned recording sessions, midnight and disconnection handling, index restart
  and cursor recovery
- Prevention of concurrent edit conflicts in the shared camera roster,
  assignment by responsible server, and stopping ingest

- Live RTSP → HLS → object store pipeline, ffmpeg 8.0.1 (previously verified
  versions: 6.1.1/6.1.2)
- Segments stored before the manifest, with every segment referenced by the
  manifest present
- Published stream is playable and the codec remains h264
- An 8-second-GOP camera produces 8-second segments against a 4-second target
  and is classified as healthy
- Camera disconnection triggers reconnection with exponential backoff (full
  jitter, `[base/2, min(base×factor^n, max)]`), without sequence rollback,
  with `EXT-X-DISCONTINUITY` inserted, and with 0 lost segments
- In the PoC with a 20-second lease TTL, an 8-second object-store outage
  recovered automatically, with 0 ffmpeg reconnections across two channels
  and 0 lost segments. If a storage outage lasts until ownership expires, the
  channel becomes `failed`; set the TTL considering both tolerated outage time
  and takeover delay
- On SIGTERM, flush remaining segments for up to 15 seconds before exit (if
  the store does not respond within that time, remaining segments are logged
  as lost); 0 zombie processes; exit code 0
- Starting 25 channels with no cameras present causes no crash
- After deleting and recreating the container and losing the local checkpoint,
  the sequence recovers from the published manifest without rollback
  (16 → 22; 0 duplicate object keys)
- If the object store cannot be read, the channel publishes nothing, does not
  start ffmpeg, and waits in `waiting_ownership` (even with a local checkpoint).
  If the manifest is not ours, it fails closed to `failed`
- With the same camera assigned to two daemons, only one starts ffmpeg and the
  other waits in `waiting_ownership`. After `docker pause` freezes the owner,
  the waiting daemon takes over after the 20-second TTL without sequence
  rollback (457 → 459). When unfrozen, the old owner detects rejected renewal,
  stops ffmpeg, and enters `failed` without touching the manifest
- CPU per channel: 0.37–0.83% of a core (four measured points at 0.5–4Mbps),
  linear scaling through 75 channels; ffmpeg 81–84% / supervisor 16–19%
- Memory per channel: 13.4–14.3 MiB, independent of bitrate (same at 0.5Mbps
  and 4Mbps)

## Contributing

See the [contribution guide](CONTRIBUTING.md) for proposing changes, setting
up a development environment, verification commands, and DCO sign-off.
Participants follow the [code of conduct](CODE_OF_CONDUCT.md). Report security
vulnerabilities through the [private reporting channel](SECURITY.md#취약점-신고)
instead of a public issue.

## License

transmux is distributed under the [Apache License 2.0](LICENSE). See
[NOTICE](NOTICE) for copyright and upstream project notices, and
[third-party notices](THIRD_PARTY_NOTICES.md) for licenses of Go dependencies,
hls.js, and runtime ffmpeg. ffmpeg and other components included in the runtime
image remain subject to their respective licenses.
