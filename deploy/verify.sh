#!/bin/sh
# End-to-end verification for the PoC stack.
#
# The checks below are the ones that actually matter in production:
#   1. transmuxd reports every channel healthy.
#   2. A manifest exists in object storage for each camera.
#   3. Every segment the manifest references really exists. A dangling
#      reference is the failure that breaks playback for all viewers at once.
#   4. The published stream is decodable and still H.264, proving no
#      transcode happened.
#   5. The long-GOP camera produces longer segments than the target without
#      being reported as faulty.

set -eu

# S3 is the object store endpoint; MINIO is still honoured for old callers.
S3="${S3:-${MINIO:-http://s3:9000}}"
BUCKET="${BUCKET:-transmux-poc}"
API="${API:-http://transmuxd:8080}"
CENTER="${CENTER:-center-01}"
CAMERAS="${CAMERAS:-cam-shortgop cam-longgop}"
VERIFY_TIMEOUT="${VERIFY_TIMEOUT:-90}"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

echo "== 1. shard health =="
case "$VERIFY_TIMEOUT" in
  ''|*[!0-9]*) fail "VERIFY_TIMEOUT must be a number of seconds" ;;
esac
deadline=$(($(date +%s) + VERIFY_TIMEOUT))
while :; do
  health=$(wget -q -T 5 -O- "$API/healthz" 2>/dev/null || true)
  # The counts appear both at the top level and under "shard"; take the first.
  total=$(echo "$health"   | tr -d ' ",' | awk -F: '/^channels_total:/{print $2; exit}')
  healthy=$(echo "$health" | tr -d ' ",' | awk -F: '/^channels_healthy:/{print $2; exit}')
  if [ "${total:-0}" -gt 0 ] && [ "$healthy" = "$total" ]; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || fail "channels did not become healthy within ${VERIFY_TIMEOUT}s: $health"
  sleep 2
done
echo "$health"
ok "$healthy/$total channels healthy"

echo "== 2. credentials must not leak through the API =="
channels=$(wget -q -O- "$API/channels")
echo "$channels" | grep -q 'rtsp://' || fail "expected a redacted source URL in /channels"
echo "$channels" | grep -qE 'rtsp://[^*"]*:[^@"]*@' && fail "an RTSP URL with credentials leaked into /channels"
ok "no credentials in /channels"

for cam in $CAMERAS; do
  echo "== camera $cam =="
  base="$S3/$BUCKET/$CENTER/$cam"
  manifest_url="$base/index.m3u8"

  manifest=$(wget -q -O- "$manifest_url") || fail "$cam: no manifest at $manifest_url"
  echo "$manifest" | grep -q '^#EXTM3U' || fail "$cam: manifest is not a playlist"
  echo "$manifest" | grep -q '#EXT-X-TARGETDURATION:' || fail "$cam: no target duration"
  echo "$manifest" | grep -q '#EXT-X-MEDIA-SEQUENCE:' || fail "$cam: no media sequence"
  echo "$manifest" | grep -q '#EXT-X-ENDLIST' && fail "$cam: a live playlist must not be closed with ENDLIST"
  ok "manifest present and well formed"

  # 3. no dangling segment references
  count=0
  longest=0
  for uri in $(echo "$manifest" | grep -v '^#' | grep -v '^$'); do
    wget -q --spider "$base/$uri" || fail "$cam: manifest references a missing object: $uri"
    count=$((count + 1))
  done
  [ "$count" -gt 0 ] || fail "$cam: manifest lists no segments"
  ok "$count/$count referenced segments exist in object storage"

  # 5. actual segment duration, which the camera GOP dictates
  longest=$(echo "$manifest" | awk -F'[:,]' '/^#EXTINF:/ {if ($2+0 > m) m=$2+0} END {printf "%.3f", m}')
  ok "longest EXTINF ${longest}s"
  target=$(echo "$manifest" | awk -F: '/^#EXT-X-TARGETDURATION:/{print $2}')
  # EXT-X-TARGETDURATION must cover the real maximum or strict players stall.
  awk -v l="$longest" -v t="$target" 'BEGIN{ if (l > t) exit 1 }' \
    || fail "$cam: EXT-X-TARGETDURATION $target is below the longest segment $longest"
  ok "target duration $target covers the longest segment"

  # 4. decodable, and still the original codec
  codec=$(timeout 30 ffprobe -v error -select_streams v:0 \
            -show_entries stream=codec_name -of csv=p=0 \
            "$manifest_url" 2>/dev/null | head -1 | tr -d '\r\n ')
  [ "$codec" = "h264" ] || fail "$cam: published codec is '$codec', expected an untouched h264"
  timeout 30 ffmpeg -hide_banner -loglevel error -i "$manifest_url" -frames:v 1 -f null - \
    >/dev/null 2>&1 || fail "$cam: the published stream could not decode a video frame"
  ok "playback via HLS works and the codec is still h264"
done

echo "== 6. long-GOP camera really does produce longer segments =="
short=$(wget -q -O- "$S3/$BUCKET/$CENTER/cam-shortgop/index.m3u8" \
         | awk -F'[:,]' '/^#EXTINF:/ {if ($2+0 > m) m=$2+0} END {printf "%.3f", m}')
long=$(wget -q -O- "$S3/$BUCKET/$CENTER/cam-longgop/index.m3u8" \
         | awk -F'[:,]' '/^#EXTINF:/ {if ($2+0 > m) m=$2+0} END {printf "%.3f", m}')
echo "  shortgop longest=${short}s  longgop longest=${long}s"
awk -v s="$short" -v l="$long" 'BEGIN{ if (l <= s) exit 1 }' \
  || fail "the 8s-GOP camera should produce longer segments than the 2s-GOP one"
ok "segment length follows the camera GOP, as expected for a pure transmux"

echo
echo "ALL CHECKS PASSED"
