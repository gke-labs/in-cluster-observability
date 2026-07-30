#!/usr/bin/env python3
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Generate meta-refresh HTML stubs for legacy Hugo URLs.

GitHub Pages doesn't support server-side redirects. Instead, we plant
static HTML at each old URL that both:
  1. Responds to bots with a canonical link to the new URL
  2. Redirects browsers via <meta http-equiv="refresh">

The stubs land in `docs/site/public/docs/...` — Astro copies public/
verbatim into dist/, so they end up served at their old URL paths.

The map below is the complete set of pages the Hugo site ever
published (site root aside, which still serves the landing page).
Idempotent; re-run after adding an entry.

Usage:
  python3 scripts/generate-redirects.py
"""
from __future__ import annotations

import json
import pathlib
import shutil
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parents[1]
DST_ROOT = REPO_ROOT / "docs/site/public/docs"
BASE = "/in-cluster-observability"

# old Hugo path under /docs/ -> new Starlight URL (with base).
# "" is /docs/ itself, which had no Starlight equivalent; it goes to
# the landing page.
REDIRECTS = {
    "": f"{BASE}/",
    "getting-started": f"{BASE}/getting-started/",
    "what-works-today": f"{BASE}/what-works-today/",
    "architecture": f"{BASE}/architecture/",
    "roadmap": f"{BASE}/roadmap/",
    "contributing": f"{BASE}/contributing/",
}

REDIRECT_HTML = """<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Moved</title>
<link rel="canonical" href="{new_url}">
<meta http-equiv="refresh" content="0; url={new_url}">
<meta name="robots" content="noindex">
<script>window.location.replace({new_url_json});</script>
</head>
<body>
<p>This page moved to <a href="{new_url}">{new_url}</a>.</p>
</body>
</html>
"""


def main() -> int:
    if DST_ROOT.exists():
        shutil.rmtree(DST_ROOT)

    for old, new_url in sorted(REDIRECTS.items()):
        stub_dir = DST_ROOT / old if old else DST_ROOT
        stub_dir.mkdir(parents=True, exist_ok=True)
        (stub_dir / "index.html").write_text(REDIRECT_HTML.format(
            new_url=new_url,
            new_url_json=json.dumps(new_url),
        ))
        print(f"  redirect  /docs/{old + '/' if old else ''}  ->  {new_url}")

    print(f"\ngenerated {len(REDIRECTS)} redirect stubs")
    return 0


if __name__ == "__main__":
    sys.exit(main())
