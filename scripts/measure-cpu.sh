#!/usr/bin/env bash
# Measure the CPU cost of one channel, precisely.
#
# Why this and not `docker stats`: docker stats reports a short rolling sample
# and rounds to two decimals, which at ~0.5% per channel is mostly noise. This
# script reads the container's cgroup v2 cpu.stat usage_usec counter, which is
# exact CPU time consumed, and divides by measured wall clock. It also splits
# the total into ffmpeg versus supervisor, and normalises by segments produced
# so cameras with different GOP lengths can be compared fairly.
#
# Two numbers matter and they are not the same:
#
#   ABSOLUTE at N=1   includes the Go runtime, GC and HTTP server, which are
#                     paid once per container regardless of channel count.
#   MARGINAL (slope)  the cost of adding one more channel. This is the figure
#                     to extrapolate to 625.
#
# The slope also answers whether per-channel cost degrades at scale. If
# hundreds of ffmpeg processes caused the scheduler thrashing that is often
# claimed, the slope would rise with N. Look at the MARGINAL column.
#
# Prerequisite: the PoC stack is up. The hd, d1 and hd720 profiles need the
# capacity sources too, so use `make poc-up-capacity` for those.
#
# Usage: scripts/measure-cpu.sh [--profile shortgop|longgop|hd] [counts...]
#        scripts/measure-cpu.sh 1 10 25 50 75

set -euo pipefail
cd "$(dirname "$0")/.."

NETWORK="${NETWORK:-transmux-poc_default}"
IMAGE="${IMAGE:-transmux:latest}"
NAME=transmux-cpu
WARMUP="${WARMUP:-45}"   # let every channel reach steady state first
WINDOW="${WINDOW:-60}"   # measurement window; longer is more precise

PROFILE=shortgop
if [ "${1:-}" = "--profile" ]; then
  PROFILE="$2"; shift 2
fi
COUNTS=("$@")
[ ${#COUNTS[@]} -gt 0 ] || COUNTS=(1 10 25 50)

case "$PROFILE" in
  shortgop) RTSP_PATH=cam-shortgop ;;   # 640x360  1.5Mbps GOP 2s
  longgop)  RTSP_PATH=cam-longgop  ;;   # 640x360  1.5Mbps GOP 8s
  hd)       RTSP_PATH=cam-1080p    ;;   # 1920x1080 4Mbps  GOP 4s
  d1)       RTSP_PATH=cam-d1       ;;   # 704x480  0.5Mbps GOP 2s  (전형적 관제 D1)
  hd720)    RTSP_PATH=cam-720p     ;;   # 1280x720 1Mbps   GOP 2s
  *) echo "unknown profile: $PROFILE" >&2; exit 2 ;;
esac

# A declared MediaMTX path with no publisher is not an error at the RTSP layer:
# transmuxd connects, receives nothing, and the run reports the CPU cost of
# idle channels. Refuse to produce that number.
#
# The check probes the stream rather than asking the MediaMTX API, which needs
# credentials in 1.9.x, and rather than trusting that a publisher container is
# running, which does not prove media is flowing.
require_publisher() {
  path="$1"
  if ! docker ps --format '{{.Names}}' | grep -qx transmux-verifier; then
    echo "the PoC stack is not running; start it with: make poc-up-capacity" >&2
    exit 2
  fi
  if docker exec transmux-verifier timeout 15 ffprobe -v error \
       -rtsp_transport tcp -i "rtsp://mediamtx:8554/$path" \
       -show_entries stream=codec_name -of csv=p=0 >/dev/null 2>&1; then
    return 0
  fi
  cat >&2 <<MSG
no publisher on RTSP path "$path" (profile $PROFILE).

The 1080p, D1 and 720p sources live behind the compose "capacity" profile so a
normal PoC run does not pay for three extra encoders. Start them with:

    make poc-up-capacity

Measuring without a publisher would report the cost of idle channels, not of
the profile.
MSG
  exit 2
}
require_publisher "$RTSP_PATH"

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

gen_config() {
  python3 - "$1" "$RTSP_PATH" <<'PY'
import json, sys
n, rtsp_path = int(sys.argv[1]), sys.argv[2]
cfg = json.load(open("configs/poc.json"))
cfg["max_channels"] = n
cfg["shard_id"] = "cpu-probe"
cfg["cameras"]["static"] = [
    {"center_id": "cpu", "camera_id": "ch-%04d" % i,
     "rtsp_url": "rtsp://mediamtx:8554/%s" % rtsp_path}
    for i in range(n)
]
json.dump(cfg, open("/tmp/transmux-cpu.json", "w"), indent=2)
PY
}

# snapshot emits key=value lines read from inside the container.
snapshot() {
  docker exec "$NAME" sh -c '
    awk "{print \$1\"=\"\$2}" /sys/fs/cgroup/cpu.stat | grep -E "^(usage|user|system)_usec="
    # /proc/uptime rather than date +%%N: busybox date has no nanoseconds and
    # silently drops the format, which would corrupt the wall-clock delta.
    echo "wall_cs=$(awk "{printf \"%d\", \$1*100}" /proc/uptime)"
    ff=0; sv=0; cs=0; np=0
    for p in $(ls /proc | grep -E "^[0-9]+$"); do
      [ -r /proc/$p/stat ] || continue
      comm=$(sed -n "s/.*(\(.*\)).*/\1/p" /proc/$p/stat)
      ticks=$(awk "{print \$14+\$15}" /proc/$p/stat 2>/dev/null || echo 0)
      case "$comm" in
        ffmpeg)    ff=$((ff+ticks)); np=$((np+1)) ;;
        transmuxd) sv=$((sv+ticks)) ;;
      esac
      if [ -r /proc/$p/status ]; then
        v=$(awk "/voluntary_ctxt_switches/{s+=\$2} END{print s+0}" /proc/$p/status)
        cs=$((cs+v))
      fi
    done
    echo "ffmpeg_ticks=$ff"
    echo "supervisor_ticks=$sv"
    echo "ctxt_switches=$cs"
    echo "ffmpeg_procs=$np"
    segs=$(wget -q -O- http://127.0.0.1:8080/metrics |
      awk "/^transmux_channel_segments_published_total\{/ {s+=\$2} END{printf \"%d\", s+0}")
    echo "segments=$segs"
  '
}

echo "profile=$PROFILE  source=$RTSP_PATH  warmup=${WARMUP}s  window=${WINDOW}s"
echo "host: $(nproc) logical CPUs, load$(cut -d' ' -f1-3 /proc/loadavg | sed 's/^/ /')"
echo
printf '%-6s %-9s %-9s %-9s %-9s %-9s %-8s %-10s %-9s\n' \
  CH CORES USER SYS FFMPEG SUPERV PROCS CORES/CH CPU_ms/SEG
printf '%s\n' "-------------------------------------------------------------------------------------"

RESULTS=()
for n in "${COUNTS[@]}"; do
  cleanup
  gen_config "$n"
  docker run -d --name "$NAME" --network "$NETWORK" \
    -v /tmp/transmux-cpu.json:/etc/transmux/config.json:ro \
    -e AWS_ACCESS_KEY_ID=minioadmin -e AWS_SECRET_ACCESS_KEY=minioadmin \
    -e AWS_REGION=us-east-1 \
    --tmpfs /run/transmux/spool:size=2g,mode=0750,uid=10001,gid=10001 \
    --tmpfs /var/lib/transmux/state:size=128m,mode=0750,uid=10001,gid=10001 \
    --ulimit nofile=65535:65535 \
    "$IMAGE" -config /etc/transmux/config.json >/dev/null

  sleep "$WARMUP"
  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
    echo "$n: container exited early"; docker logs "$NAME" 2>&1 | tail -3; continue
  fi

  A=$(snapshot); sleep "$WINDOW"; B=$(snapshot)

  read -r cores user sys ff sv procs perseg <<EOF
$(python3 - <<PY
def parse(text):
    d = {}
    for line in text.strip().splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            try: d[k] = int(v)
            except ValueError: pass
    return d

a = parse("""$A""")
b = parse("""$B""")
TICK = 100.0

secs = (b["wall_cs"] - a["wall_cs"]) / 100.0
cores = (b["usage_usec"] - a["usage_usec"]) / 1e6 / secs
user  = (b["user_usec"]  - a["user_usec"])  / 1e6 / secs
sysc  = (b["system_usec"]- a["system_usec"])/ 1e6 / secs
ff    = (b["ffmpeg_ticks"] - a["ffmpeg_ticks"]) / TICK / secs
sv    = (b["supervisor_ticks"] - a["supervisor_ticks"]) / TICK / secs
segs  = b["segments"] - a["segments"]
# CPU milliseconds spent per published segment: normalises GOP differences.
perseg = (cores * secs * 1000.0 / segs) if segs > 0 else float("nan")
print("%.4f %.4f %.4f %.4f %.4f %d %.1f" % (
    cores, user, sysc, ff, sv, b["ffmpeg_procs"], perseg))
PY
)
EOF

  per_ch=$(python3 -c "print('%.4f' % ($cores/$n))")
  printf '%-6s %-9s %-9s %-9s %-9s %-9s %-8s %-10s %-9s\n' \
    "$n" "$cores" "$user" "$sys" "$ff" "$sv" "$procs" "$per_ch" "$perseg"
  RESULTS+=("$n:$cores")
done

cleanup
echo
python3 - "${RESULTS[@]}" <<'PY'
import sys
pts = [(int(p.split(":")[0]), float(p.split(":")[1])) for p in sys.argv[1:]]
if len(pts) < 2:
    sys.exit(0)
# Least squares fit cores = a + b*channels. b is the marginal cost, a is the
# fixed per-container cost that does not scale with channel count.
n = len(pts)
sx = sum(x for x, _ in pts); sy = sum(y for _, y in pts)
sxx = sum(x * x for x, _ in pts); sxy = sum(x * y for x, y in pts)
b = (n * sxy - sx * sy) / (n * sxx - sx * sx)
a = (sy - b * sx) / n
print("linear fit:  cores = %.4f + %.5f x channels" % (a, b))
print("  fixed per-container overhead : %.3f cores" % a)
print("  MARGINAL per channel         : %.5f cores  (%.3f%% of one core)" % (b, b * 100))
for target in (100, 625):
    print("  extrapolated %3d channels    : %.2f cores" % (target, a + b * target))
# Superlinearity check: compare the slope over the first and last intervals.
if len(pts) >= 3:
    pts.sort()
    lo = (pts[1][1] - pts[0][1]) / (pts[1][0] - pts[0][0])
    hi = (pts[-1][1] - pts[-2][1]) / (pts[-1][0] - pts[-2][0])
    print("  slope low-N %.5f vs high-N %.5f  ->  %s" % (
        lo, hi, "degrades with scale" if hi > lo * 1.25 else "scales linearly"))
PY
