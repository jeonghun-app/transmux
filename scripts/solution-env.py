#!/usr/bin/env python3
"""Create local-only credentials without overwriting an existing setup."""
import os
from pathlib import Path
import secrets

root = Path(__file__).resolve().parent.parent
filename = root / ".env.solution"
values = {
    "TRANSMUX_AUTH_SECRET": secrets.token_urlsafe(48),
    "TRANSMUX_ADMIN_PASSWORD": secrets.token_urlsafe(18),
    "TRANSMUX_VIEWER_PASSWORD": secrets.token_urlsafe(18),
    "TRANSMUX_MINIO_PASSWORD": secrets.token_urlsafe(32),
    "TRANSMUX_HTTP_PORT": "8090",
    "TRANSMUX_PUBLIC_URL": "http://localhost:8090",
}
try:
    fd = os.open(filename, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except FileExistsError:
    print(".env.solution already exists; credentials were preserved.")
else:
    with os.fdopen(fd, "w") as output:
        output.write("# Local demonstration credentials. Never commit this file.\n")
        for key, value in values.items():
            output.write(f"{key}={value}\n")
    print("Created .env.solution (mode 0600). Login passwords are stored there.")
