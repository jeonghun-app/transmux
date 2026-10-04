#!/usr/bin/env python3
"""Create local-only credentials without overwriting an existing setup."""
import fcntl
import os
from pathlib import Path
import re
import secrets
import tempfile

root = Path(__file__).resolve().parent.parent
filename = root / ".env.solution"
values = {
    "TRANSMUX_AUTH_SECRET": secrets.token_urlsafe(48),
    "TRANSMUX_ADMIN_PASSWORD": secrets.token_urlsafe(18),
    "TRANSMUX_VIEWER_PASSWORD": secrets.token_urlsafe(18),
    "TRANSMUX_S3_SECRET_KEY": secrets.token_urlsafe(32),
    "TRANSMUX_HTTP_PORT": "8090",
    "TRANSMUX_PUBLIC_URL": "http://localhost:8090",
}
# A key as Compose reads it: optional "export", spaces around "=". Values are
# never parsed, because they are never rewritten.
KEY = re.compile(r"\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=")


def write_atomically(content):
    """Replace the file with a 0600 copy so a reader never sees half of it."""
    fd, temporary = tempfile.mkstemp(dir=root, prefix=".env.solution.", suffix=".tmp")
    try:
        with os.fdopen(fd, "w") as output:
            output.write(content)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, filename)
    except BaseException:
        os.unlink(temporary)
        raise


# Serialise concurrent runs so two of them cannot add different secrets. The
# lock is on the directory because the file itself is replaced.
directory = os.open(root, os.O_RDONLY)
try:
    fcntl.flock(directory, fcntl.LOCK_EX)
    if not filename.exists():
        write_atomically("# Local demonstration credentials. Never commit this file.\n"
                         + "".join(f"{key}={value}\n" for key, value in values.items()))
        print("Created .env.solution (mode 0600). Login passwords are stored there.")
    else:
        # Keep every existing line, but add keys introduced since the file
        # was written so an older setup does not fail on a missing variable.
        existing = filename.read_text()
        present = {match.group(1) for match in map(KEY.match, existing.splitlines()) if match}
        missing = {key: value for key, value in values.items() if key not in present}
        if missing:
            separator = "" if not existing or existing.endswith("\n") else "\n"
            write_atomically(existing + separator
                             + "".join(f"{key}={value}\n" for key, value in missing.items()))
            print(".env.solution already exists; credentials were preserved and "
                  f"{', '.join(missing)} added.")
        else:
            print(".env.solution already exists; credentials were preserved.")
finally:
    os.close(directory)
