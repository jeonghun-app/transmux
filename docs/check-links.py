#!/usr/bin/env python3
"""Check relative links and GitHub heading anchors in repository Markdown."""
import html
from html.parser import HTMLParser
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parent.parent


def prose(text):
    lines, fence = [], ""
    for line in text.splitlines():
        marker = re.match(r"^\s{0,3}(`{3,}|~{3,})", line)
        if marker:
            run = marker[1]
            if not fence:
                fence = run
            elif run[0] == fence[0] and len(run) >= len(fence):
                fence = ""
            lines.append("")
        else:
            lines.append("" if fence else line)
    return "\n".join(lines)


class HTMLLinks(HTMLParser):
    def __init__(self, text):
        super().__init__()
        self.links, self.anchors = [], set()
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        for name, value in attrs:
            if value is None:
                continue
            if name in ("href", "src"):
                self.links.append((self.getpos()[0], value))
            if name == "id" or (tag == "a" and name == "name"):
                self.anchors.add(value)


def anchors(text):
    found, used = set(), set()
    lines = prose(text).splitlines()
    for number, line in enumerate(lines):
        heading = re.match(r"^ {0,3}#{1,6}\s+(.+?)(?:\s+#+)?\s*$", line)
        title = heading[1] if heading else None
        if number and re.fullmatch(r" {0,3}(?:=+|-+)\s*", line):
            title = lines[number - 1].strip()
        if not title:
            continue
        title = re.sub(r"!?\[([^]]*)\]\([^)]*\)", r"\1", title)
        title = html.unescape(re.sub(r"<[^>]*>", "", title))
        slug = re.sub(r"[^\w\s-]", "", title.lower())
        slug = re.sub(r"\s", "-", slug)
        candidate, suffix = slug, 0
        while candidate in used:
            suffix += 1
            candidate = f"{slug}-{suffix}"
        used.add(candidate)
        found.add(candidate)
    return found | HTMLLinks(prose(text)).anchors


def links(text):
    text = prose(text)
    # Inline code examples are not rendered links.
    text = re.sub(r"(`+)(.+?)\1", lambda m: " " * len(m[0]), text)
    patterns = (
        r"\]\(\s*(<[^>\n]+>|(?:[^()\s]|\([^()\s]*\))+)(?:\s+['\"][^\n]*?['\"])?\s*\)",
        r"(?m)^ {0,3}\[[^]\n]+\]:\s*(<[^>\n]+>|\S+)",
    )
    for pattern in patterns:
        for match in re.finditer(pattern, text):
            yield text.count("\n", 0, match.start()) + 1, match[1].strip("<>")
    yield from HTMLLinks(text).links


def main():
    names = subprocess.check_output(
        ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.md"],
        cwd=ROOT,
    ).decode().split("\0")
    documents = {
        ROOT / name: (ROOT / name).read_text()
        for name in sorted(set(names))
        if name and (ROOT / name).is_file()
    }
    heading_ids = {path: anchors(text) for path, text in documents.items()}
    failures, checked, fragments = [], 0, 0
    for source, text in documents.items():
        for line, raw in links(text):
            url = urlsplit(html.unescape(raw))
            if url.scheme or url.netloc:
                continue
            checked += 1
            path = unquote(url.path)
            target = ((ROOT / path.lstrip("/")) if path.startswith("/")
                      else (source.parent / path) if path else source).resolve()
            error = ""
            if not target.is_relative_to(ROOT) or not target.exists():
                error = "missing target"
            elif url.fragment and target.suffix.lower() == ".md":
                fragments += 1
                if unquote(url.fragment) not in heading_ids.get(target, set()):
                    error = "missing heading or anchor"
            if error:
                failures.append(f"{source.relative_to(ROOT)}:{line}: {error}: {raw}")
    for failure in failures:
        print(failure)
    print(f"Checked {len(documents)} Markdown files, {checked} relative links, "
          f"{fragments} anchors: {len(failures)} errors.")
    return bool(failures)


if __name__ == "__main__":
    sys.exit(main())
