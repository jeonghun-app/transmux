#!/usr/bin/env python3
"""Exercise the local private-bucket stack, including real media decoding."""
import argparse
import datetime as dt
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from xml.etree import ElementTree

ROOT = Path(__file__).resolve().parent.parent


def credentials(filename):
    # Supported .env syntax: KEY=value, an optional leading "export", spaces
    # around "=", a UTF-8 BOM, comments and simple '...' or "..." quoting.
    # Escapes, variable references and multi-line values are not supported;
    # the values make solution-env generates are URL-safe and unaffected.
    values = {}
    for line in Path(filename).read_text(encoding="utf-8-sig").splitlines():
        match = re.match(r"\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=\s*(.*?)\s*$", line)
        if not match:
            continue
        key, value = match.groups()
        quoted = re.fullmatch(r"""(['"])(.*?)\1(?:\s+#.*)?""", value)
        value = quoted.group(2) if quoted else re.sub(r"\s+#.*$", "", value)
        values[key] = value
    return values


def request(url, token="", body=None, headers=None, expected=200):
    combined = {"Accept": "application/json", **(headers or {})}
    if token:
        combined["Authorization"] = "Bearer " + token
    if body is not None:
        combined["Content-Type"] = "application/json"
    req = urllib.request.Request(
        url, data=None if body is None else json.dumps(body).encode(), headers=combined
    )
    try:
        response = urllib.request.urlopen(req, timeout=30)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        data = response.read()
        if response.status != expected:
            # Media URLs contain capabilities: never print them in failures.
            raise AssertionError(f"HTTP {response.status}; expected {expected}")
        return data, response.headers


def as_json(url, token="", body=None, expected=200):
    return json.loads(request(url, token, body, expected=expected)[0])


def eventually(callback, timeout=120):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            result = callback()
            if result:
                return result
        except (AssertionError, urllib.error.URLError, TimeoutError, ConnectionError) as error:
            last = error
        time.sleep(1)
    raise AssertionError(f"Condition did not become ready within {timeout}s: {last}")


def inspect_playlist(raw):
    segments, initializations = [], set()
    pdt, duration = None, None
    for line in raw.decode().splitlines():
        if line.startswith("#EXT-X-PROGRAM-DATE-TIME:"):
            pdt = dt.datetime.fromisoformat(line.split(":", 1)[1].replace("Z", "+00:00"))
        elif line.startswith("#EXTINF:"):
            duration = float(line.split(":", 1)[1].split(",")[0])
        elif line.startswith("#EXT-X-MAP:"):
            initializations.add(re.fullmatch(r'#EXT-X-MAP:URI="([^"]+)"', line).group(1))
        elif line and not line.startswith("#"):
            assert re.fullmatch(r"\d{4}/\d{2}/\d{2}/seg-\d+-\d+\.(ts|m4s)", line), "unsafe media URI"
            assert pdt is not None and duration > 0, "missing capture timestamp or duration"
            segments.append({"uri": line, "start": pdt, "duration": duration})
    assert segments, "empty media playlist"
    return segments, initializations


def run_media(image, directory, program, arguments):
    result = subprocess.run(
        ["docker", "run", "--rm", "--network", "none", "--user", f"{os.getuid()}:{os.getgid()}",
         "--cap-drop", "ALL", "-v", f"{directory}:/clips:ro",
         "--entrypoint", program, image, *arguments],
        capture_output=True, text=True, timeout=60, check=False,
    )
    if result.returncode:
        raise AssertionError(f"{program} failed: {result.stderr[-1500:]}")
    return result.stdout


# Signed GetObjectTagging, run inside the storage container because S3 is not
# published to the host. The secret comes from that container's environment
# and reaches curl on stdin, never argv. curl config values are quoted, so
# " and \ are escaped; a line break cannot be represented and is refused.
TAGS_SCRIPT = (
    "nl='\n'; cr=$(printf '\\r'); "
    'case "$RUSTFS_ACCESS_KEY$RUSTFS_SECRET_KEY" in *"$nl"*|*"$cr"*) '
    'echo "S3 credentials must not contain line breaks" >&2; exit 3 ;; esac; '
    'esc() { printf "%s" "$1" | sed \'s/[\\\\"]/\\\\&/g\'; }; '
    'printf \'user = "%s:%s"\\n\' "$(esc "$RUSTFS_ACCESS_KEY")" "$(esc "$RUSTFS_SECRET_KEY")" '
    '| curl -fsS -K - --aws-sigv4 "aws:amz:us-east-1:s3" '
    '"http://127.0.0.1:9000/transmux/$1?tagging"'
)


def object_tags(args, key):
    result = subprocess.run(
        ["docker", "compose", "--env-file", args.env_file, "-f",
         str(ROOT / "deploy/docker-compose.solution.yml"), "-p", args.project,
         "exec", "-T", "s3", "sh", "-c", TAGS_SCRIPT,
         "verify", urllib.parse.quote(key, safe="/")],
        capture_output=True, text=True, timeout=15, check=False,
    )
    assert result.returncode != 3, "S3 credentials must not contain line breaks"
    assert result.returncode == 0, "could not inspect media lifecycle tags"
    namespace = {"s3": "http://s3.amazonaws.com/doc/2006-03-01/"}
    return {
        tag.findtext("s3:Key", namespaces=namespace): tag.findtext("s3:Value", namespaces=namespace)
        for tag in ElementTree.fromstring(result.stdout).iterfind("s3:TagSet/s3:Tag", namespace)
    }


def download_playlist(url, raw, directory):
    segments, inits = inspect_playlist(raw)
    for uri in [entry["uri"] for entry in segments] + sorted(inits):
        destination = directory / uri
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(request(urllib.parse.urljoin(url, uri))[0])
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "index.m3u8").write_bytes(
        raw if raw.endswith(b"#EXT-X-ENDLIST\n") else raw + b"#EXT-X-ENDLIST\n"
    )


def verify_restart(args, base, token, archive, original):
    compose = ["docker", "compose", "--env-file", args.env_file, "-f",
               str(ROOT / "deploy/docker-compose.solution.yml"), "-p", args.project]
    for service in ["playbackd", "transmuxd"]:
        before = dt.datetime.now(dt.timezone.utc)
        subprocess.run(compose + [
            "up", "-d", "--no-build", "--no-deps", "--force-recreate", service
        ], check=True, capture_output=True, timeout=60)
        eventually(lambda: as_json(base + "/readyz").get("status") == "ready")
        if service == "playbackd":
            assert request(archive["url"])[0] == original, "fixed VOD changed after restart"
            entries, _ = inspect_playlist(original)
            request(urllib.parse.urljoin(archive["url"], entries[0]["uri"]),
                    headers={"Range": "bytes=0-15"}, expected=206)
        else:
            old_sequence = int(inspect_playlist(original)[0][-1]["uri"].split("seg-")[1].split("-")[0])

            def new_capture():
                stream = as_json(base + "/v1/playback-sessions", token, {
                    "center_id": "demo-center", "camera_ids": ["hevc"], "mode": "live",
                }, expected=201)["streams"][0]
                entries, _ = inspect_playlist(request(stream["url"])[0])
                latest = entries[-1]
                sequence = int(latest["uri"].split("seg-")[1].split("-")[0])
                return sequence if sequence > old_sequence and latest["start"] > before else None

            latest = eventually(new_capture)
    return {
        "fixed_vod_after_playback_restart": True,
        "old_capability_range_after_restart": True,
        "ingest_sequence_before": old_sequence,
        "ingest_sequence_after": latest,
        "new_capture_after_ingest_restart": True,
    }


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--env-file", default=str(ROOT / ".env.solution"))
    parser.add_argument("--base-url")
    parser.add_argument("--project", default="transmux-solution")
    parser.add_argument("--image", default="transmux-solution:latest")
    parser.add_argument("--restart", action="store_true",
                        help="also recreate the local ingest/playback containers and verify recovery")
    args = parser.parse_args()
    env = credentials(args.env_file)
    base = (args.base_url or env.get("TRANSMUX_PUBLIC_URL", "http://localhost:8090")).rstrip("/")
    eventually(lambda: as_json(base + "/readyz").get("status") == "ready")
    admin = as_json(base + "/v1/login", body={
        "username": "admin", "password": env["TRANSMUX_ADMIN_PASSWORD"]
    })["access_token"]
    viewer = as_json(base + "/v1/login", body={
        "username": "viewer", "password": env["TRANSMUX_VIEWER_PASSWORD"]
    })["access_token"]
    request(base + "/v1/cameras", expected=401)
    catalog = as_json(base + "/v1/cameras", admin)["cameras"]
    assert {"entrance", "warehouse", "hevc"}.issubset({cam["camera_id"] for cam in catalog})
    viewer_catalog = as_json(base + "/v1/cameras", viewer)["cameras"]
    assert {cam["camera_id"] for cam in viewer_catalog} == {"entrance", "warehouse"}
    assert "rtsp" not in json.dumps(catalog).lower(), "catalog leaked an RTSP source"
    summary = {"private_bucket": False, "authentication": True, "lifecycle_tags": True, "cameras": []}
    with tempfile.TemporaryDirectory(prefix="transmux-solution-verify-") as temporary:
        directory = Path(temporary)
        for camera, codec in [("entrance", "h264"), ("warehouse", "h264"), ("hevc", "hevc")]:
            live = eventually(lambda: as_json(base + "/v1/playback-sessions", admin, {
                "center_id": "demo-center", "camera_ids": [camera], "mode": "live"
            }, expected=201)["streams"][0])
            live_url = live["url"]
            raw, headers = request(live_url)
            assert headers["Cache-Control"] == "no-store"
            segments, inits = inspect_playlist(raw)
            other = "warehouse" if camera == "entrance" else "entrance"
            request(live_url.replace(f"/demo-center/{camera}/", f"/demo-center/{other}/"), expected=403)
            request(urllib.parse.urljoin(live_url, "_transmux/lease.json"), expected=404)
            media_url = urllib.parse.urljoin(live_url, segments[0]["uri"])
            partial, partial_headers = request(media_url, headers={"Range": "bytes=0-15"}, expected=206)
            assert len(partial) == 16 and partial_headers["Content-Range"].startswith("bytes 0-15/")
            prefix = f"recordings/demo-center/{camera}/"
            assert object_tags(args, prefix + segments[-1]["uri"]) == {"transmux-kind": "segment"}
            for uri in inits:
                assert object_tags(args, prefix + uri) == {"transmux-kind": "init"}
            for control in ["index.m3u8", "_transmux/lease.json"]:
                assert not object_tags(args, prefix + control), "control object is subject to media expiry"
            if camera == "entrance":
                assert not object_tags(args, "_transmux/rosters/solution.json")
                object_url = "http://s3:9000/transmux/recordings/demo-center/entrance/" + segments[0]["uri"]
                denied = subprocess.run([
                    "docker", "compose", "--env-file", args.env_file, "-f",
                    str(ROOT / "deploy/docker-compose.solution.yml"), "-p", args.project,
                    "exec", "-T", "playbackd", "wget", "-S", "-O", "/dev/null", object_url
                ], capture_output=True, text=True, timeout=15, check=False)
                assert denied.returncode and "403" in denied.stderr, "S3 origin allows anonymous media access"
                summary["private_bucket"] = True
            # Download authenticated live media and verify a real frame,
            # rather than accepting a playlist or ffprobe header alone.
            live_dir = directory / camera
            download_playlist(live_url, raw, live_dir)
            run_media(args.image, directory, "ffmpeg", [
                "-nostdin", "-v", "error", "-protocol_whitelist", "file",
                "-allowed_extensions", "ALL", "-i", f"/clips/{camera}/index.m3u8",
                "-frames:v", "1", "-frames:a", "1", "-f", "null", "-"
            ])
            if camera == "hevc":
                live_probe = json.loads(run_media(args.image, directory, "ffprobe", [
                    "-v", "error", "-show_entries", "stream=codec_name,codec_tag_string",
                    "-of", "json", f"/clips/{camera}/index.m3u8"
                ]))
                assert any(stream.get("codec_name") == "hevc" and stream.get("codec_tag_string") == "hvc1"
                           for stream in live_probe["streams"]), "HEVC live media lacks hvc1"
            first = segments[0]["start"]
            last = segments[-1]["start"] + dt.timedelta(seconds=segments[-1]["duration"])
            query = {"center_id": "demo-center", "camera_id": camera,
                     "start": first.isoformat(), "end": last.isoformat()}
            eventually(lambda: as_json(base + "/v1/recordings?" + urllib.parse.urlencode(query), admin)["periods"])
            archive = eventually(lambda: as_json(base + "/v1/playback-sessions", admin, {
                "center_id": "demo-center", "camera_ids": [camera], "mode": "recording",
                "start": query["start"], "end": query["end"],
            }, expected=201)["streams"][0])
            vod = request(archive["url"])[0]
            assert vod.endswith(b"#EXT-X-ENDLIST\n"), "recording playlist is not finite"
            download_playlist(archive["url"], vod, directory / f"{camera}-archive")
            run_media(args.image, directory, "ffmpeg", [
                "-nostdin", "-v", "error", "-protocol_whitelist", "file",
                "-allowed_extensions", "ALL", "-i", f"/clips/{camera}-archive/index.m3u8",
                "-frames:v", "2", "-frames:a", "2", "-f", "null", "-"
            ])
            request(base + "/v1/exports", viewer, query, expected=403)
            job = as_json(base + "/v1/exports", admin, query, expected=202)

            def ready_export():
                status = as_json(base + "/v1/exports/" + job["id"], admin)
                if status["state"] == "failed":
                    raise RuntimeError(f"{camera} export failed: {status.get('error')}")
                return status if status["state"] == "ready" else None

            result = eventually(ready_export, timeout=120)
            clip, clip_headers = request(result["url"])
            assert "attachment" in clip_headers["Content-Disposition"]
            assert clip_headers["X-Recording-Start"] and clip_headers["X-Recording-End"]
            filename = f"{camera}.mp4"
            (directory / filename).write_bytes(clip)
            probe = json.loads(run_media(args.image, directory, "ffprobe", [
                "-v", "error", "-show_entries", "stream=codec_name,codec_type,codec_tag_string", "-of", "json", f"/clips/{filename}"
            ]))
            codecs = {stream["codec_type"]: stream["codec_name"] for stream in probe["streams"]}
            assert codecs.get("video") == codec and codecs.get("audio") == "aac", codecs
            video_tag = next(stream["codec_tag_string"] for stream in probe["streams"] if stream["codec_type"] == "video")
            assert video_tag == ("hvc1" if codec == "hevc" else "avc1"), "incompatible MP4 video sample entry"
            run_media(args.image, directory, "ffmpeg", [
                "-nostdin", "-v", "error", "-i", f"/clips/{filename}",
                "-frames:v", "2", "-frames:a", "2", "-f", "null", "-"
            ])
            summary["cameras"].append({
                "camera_id": camera, "video": codec, "audio": "aac", "init_maps": len(inits),
                "video_tag": video_tag,
                "segment_durations": [entry["duration"] for entry in segments],
                "export_bytes": len(clip), "decoded_live_archive_and_mp4": True,
            })
            print(f"PASS {camera}: private live, Range, archive, {codec}/AAC MP4 decoded", flush=True)
        if args.restart:
            summary["restart"] = verify_restart(args, base, admin, archive, vod)
            print("PASS restart: fixed VOD and authenticated Range survive; new ingest sequences advance", flush=True)
    print(json.dumps(summary, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
