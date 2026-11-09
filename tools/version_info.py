#!/usr/bin/env python3
"""Version information of the web UI as JSON (docs/04 §19): git describe, tag, branch,
commit and its date. Written to version.json by the Pages deploy (.github/workflows/pages.yml)
and served live by the dev server (tools/serve.py), so the start page can show which
version it is. Not committed: a committed stamp would always describe the previous commit.

Usage: python3 tools/version_info.py [--source pages|dev-server]
"""
import datetime
import json
import os
import subprocess
import sys

ROOT = os.path.normpath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))


def git(*args):
    try:
        return subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True, check=True, timeout=5).stdout.strip()
    except Exception:
        return ""


def info(source="dev-server"):
    commit = git("rev-parse", "HEAD")
    if not commit:
        return {"source": source, "error": "no git checkout"}
    # CI checks out a detached HEAD: the branch comes from the environment there
    branch = os.environ.get("GITHUB_REF_NAME") if os.environ.get("GITHUB_REF_TYPE") == "branch" else ""
    branch = branch or git("rev-parse", "--abbrev-ref", "HEAD")
    dirty = source != "pages" and bool(git("status", "--porcelain", "--untracked-files=no"))
    return {
        "source": source,
        "describe": git("describe", "--tags", "--always", "--dirty") if source != "pages" else git("describe", "--tags", "--always"),
        "tag": git("describe", "--tags", "--exact-match", "HEAD"),
        "branch": "" if branch == "HEAD" else branch,
        "commit": commit,
        "short": commit[:7],
        "date": git("log", "-1", "--format=%cI"),
        "dirty": dirty,
        "built": datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat(),
    }


if __name__ == "__main__":
    src = sys.argv[sys.argv.index("--source") + 1] if "--source" in sys.argv else "dev-server"
    json.dump(info(src), sys.stdout, indent=1)
    sys.stdout.write("\n")
