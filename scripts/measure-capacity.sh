#!/usr/bin/env bash
# Measure the real marginal cost of a channel.
#
# Answers the question the requirements hinge on: how many channels actually
# fit in a given CPU and memory budget. It runs a throwaway transmuxd on the
# PoC network with N channels and reports the container's cgroup memory, which
# is the number a container limit is enforced against. Summing per-process RSS
# overcounts badly because every ffmpeg shares the same libavcodec pages.
#
# Caveat, stated up front: every channel pulls from the same two MediaMTX
# paths. That is an accurate proxy for muxer, process and upload cost, but it
# understates real cameras, which each bring their own TCP session, jitter and
# bitrate variation. Treat the result as a floor, not a guarantee.
#
# Prerequisite: the PoC stack is up (make poc-up).
#
# Usage: scripts/measure-capacity.sh 2 10 25 50

set -euo pipefail
cd "$(dirname "$0")/.."

NETWORK="${NETWORK:-transmux-poc_default}"
IMAGE="${IMAGE:-transmux:latest}"
NAME=transmux-load
SETTLE="${SETTLE:-70}"
COUNTS=("$@")
[ ${#COUNTS[@]} -gt 0 ] || COUNTS=(2 10 25 50)

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

gen_config() {
  python3 - "$1" <<'PY'
import json, sys
n = int(sys.argv[1])
cfg = json.load(open("configs/poc.json"))
cfg["max_channels"] = n
cfg["shard_id"] = "load-shard"
paths = ["cam-shortgop", "cam-longgop"]
cfg["cameras"]["static"] = [
    {"center_id": "load", "camera_id": "ch-%04d" % i,
     "rtsp_url": "rtsp://mediamtx:8554/%s" % paths[i % len(paths)]}
    for i in range(n)
]
json.dump(cfg, open("/tmp/transmux-load.json", "w"), indent=2)
PY
}

printf '%-9s %-13s %-9s %-12s %-7s %-6s %-8s %-9s\n' \
  CHANNELS CGROUP_MEM CPU_PCT MARGINAL/CH PROCS FDS SPOOL PUBLISHED

prev_mb=0; prev_n=0
for n in "${COUNTS[@]}"; do
  cleanup
  gen_config "$n"
  # Spool sized generously so tmpfs pressure is not what limits the run.
  docker run -d --name "$NAME" --network "$NETWORK" \
    -v /tmp/transmux-load.json:/etc/transmux/config.json:ro \
    -e AWS_ACCESS_KEY_ID=minioadmin -e AWS_SECRET_ACCESS_KEY=minioadmin \
    -e AWS_REGION=us-east-1 \
    --tmpfs /run/transmux/spool:size=2g,mode=0750,uid=10001,gid=10001 \
    --tmpfs /var/lib/transmux/state:size=128m,mode=0750,uid=10001,gid=10001 \
    --ulimit nofile=65535:65535 \
    "$IMAGE" -config /etc/transmux/config.json >/dev/null

  sleep "$SETTLE"

  if ! docker ps --format '{{.Names}}' | grep -qx "$NAME"; then
    echo "$n channels: container exited early"
    docker logs "$NAME" 2>&1 | tail -5
    continue
  fi

  mem=$(docker stats --no-stream --format '{{.MemUsage}}' "$NAME" | awk '{print $1}')
  cpu=$(docker stats --no-stream --format '{{.CPUPerc}}' "$NAME")
  procs=$(docker exec "$NAME" sh -c 'ps -o pid= | wc -l' | tr -d ' ')
  fds=$(docker exec "$NAME" sh -c \
    'total=0; for p in $(ls /proc | grep -E "^[0-9]+$"); do total=$((total + $(ls /proc/$p/fd 2>/dev/null | wc -l))); done; echo $total')
  spool=$(docker exec "$NAME" sh -c 'du -sh /run/transmux/spool 2>/dev/null | cut -f1' | tr -d ' ')
  published=$(docker exec "$NAME" sh -c \
    'wget -q -O- http://127.0.0.1:8080/healthz' | tr -d ' ",' | awk -F: '/^channels_healthy:/{print $2; exit}')

  mb=$(python3 -c "
v='$mem'
print(float(v.replace('MiB','')) if 'MiB' in v else float(v.replace('GiB',''))*1024)")
  if [ "$prev_n" -gt 0 ]; then
    marg=$(python3 -c "print('%.2f' % (($mb - $prev_mb) / ($n - $prev_n)))")
  else
    marg="-"
  fi
  printf '%-9s %-13s %-9s %-12s %-7s %-6s %-8s %-9s\n' \
    "$n" "$mem" "$cpu" "${marg}MiB" "$procs" "$fds" "$spool" "${published:-?}"
  prev_mb=$mb; prev_n=$n
done

echo
echo "MARGINAL/CH is the added cgroup memory per extra channel; that is the"
echo "figure to extrapolate. The absolute total also contains the shared"
echo "ffmpeg text pages and the Go runtime, which are paid only once."
