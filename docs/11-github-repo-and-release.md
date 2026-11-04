# 11 – GitHub repository, GitHub Pages hosting and release flow (2026-11-04)

**Decisions:**
- The whole project lives in **one GitHub repository**: firmware, Go library + ctpbench, web UI, test vectors, docs.
- The web UI is served by **GitHub Pages from the repository root** (branch `main`, folder `/`). `index.html` sits at the repo root.
- **Users always use the latest version.** Pages always serves the latest release, and `go install …@latest` always gets the latest ctpbench. If the dongle's firmware doesn't match, the UI/CLI tells the user and offers the update (doc 03 §11, doc 04 §4.1). There are no per-version web URLs.

This doc is normative for repo layout, hosting and release. It supersedes the layouts sketched in docs 05–07, which now refer here.

---

## 1. Repository layout

```
/                              ← repo root = GitHub Pages site root
├── index.html                 web UI entry (doc 04)
├── .nojekyll                  disable Jekyll so every file is served as-is
├── sw.js                      Service Worker (must sit at root to control the whole site)
├── manifest.webmanifest       PWA manifest (start_url "./", scope "./")
├── favicon.svg
├── css/  js/                  web UI (vanilla, no build step)
├── fw/                        CURRENT release firmware bundle
│   ├── manifest.json          version, build_id, protocol_ver, idf, parts+offsets+sha256 (doc 04 §11.1)
│   ├── bootloader.bin  partition-table.bin  ctk.bin
│   └── embed.go               Go package `fw`: //go:embed manifest.json *.bin (for ctpbench)
├── go.mod                     module github.com/<owner>/capturetonkeli   (repo root = Go module root)
├── ctp/ …                     Go library packages (doc 05; was host/go/ctp)
├── cmd/ctpbench/              test bench (doc 06)
├── conformance/               conformance suite (importable)
├── testvectors/               shared golden vectors (Go generates; C, JS, Go consume)
├── firmware/                  ESP-IDF project (doc 07 §2 layout unchanged inside)
│   ├── idf-version.txt  idf-api.txt  sdkconfig.defaults  …
├── test/js/                   JS unit tests (node --test, no npm packages)
├── docs/                      specs 01–11 (mirrors the project research docs)
├── .github/workflows/         ci.yml, release.yml
├── Makefile                   top-level entry points (doc 07 §7 targets + web/go targets)
├── CLAUDE.md  README.md  LICENSE
```
- **Everything in the repo is publicly reachable** under the Pages URL: sources, docs, firmware. That's acceptable for an open project. The Service Worker precaches only the explicit web-app file list (§3), never the whole repo.
- `fw/` holds **only the current release**. Older releases are kept as GitHub Release assets (§4).
- Go's `go:embed` cannot reach parent directories. That is why the Go module root is the repo root and `fw/embed.go` exposes the bundle as package `github.com/<owner>/capturetonkeli/fw`. Pages serves `embed.go` as a plain file, which is harmless.

## 2. GitHub Pages specifics for the web UI (additions to doc 04)

- **Site URL is a sub-path:** `https://<owner>.github.io/capturetonkeli/`. Therefore:
  - all URLs in HTML/JS/CSS/manifest are **relative** (`js/main.js`, `./fw/manifest.json`), never `/js/...`;
  - Service Worker registered as `navigator.serviceWorker.register('./sw.js', {scope: './'})`;
  - Worker created with `new Worker(new URL('./js/worker.js', import.meta.url), {type: 'module'})`.
  - A custom domain later works unchanged.
- HTTPS is provided by Pages, which satisfies the secure-context requirement for Web Serial / WebUSB.
- Pages can't set custom HTTP headers. Nothing in doc 04 needs them (no COOP/COEP, since we don't use SharedArrayBuffer). Any future feature that needs headers is out of scope.
- `.nojekyll` must exist. Otherwise Jekyll processing skips files and folders starting with `_` and slows deploys.
- Pages deploys automatically on every push to `main`. **`main` is therefore always the live release** (§4).

## 3. "Always latest" in the browser: Service Worker update policy

Cache-first forever would pin users to an old UI. Policy:
- **Precache list** (`sw.js` constant): index.html, css, js, fw/* files.
- **The cache name includes the release build_id** (read from `fw/manifest.json` at SW install), so every release gets a fresh cache.
- **Navigation and `fw/manifest.json`: network-first** with a 3 s timeout, falling back to the cache (offline/field use).
- Everything else: cache-first within the current release cache.
- **Update detection:** on page load and every 30 min the page fetches `fw/manifest.json` (network-first). If the build_id differs from the running page's, the new SW installs in the background and the page shows a banner: "New version 1.4.0 available – Reload". **Never reload automatically while capturing or recording**; when idle (not connected) it reloads on the next tap of Connect.
- **Firmware pairing follows the page:** the page's own `fw/manifest.json` defines "expected firmware". A user who reloads to the latest page is then offered the matching firmware on the next connect (doc 04 §4.1).
- **Old dongle (common case):** dongle MAJOR.MINOR < page → dialog with **Update** as the default button.
- **Newer dongle** (developer with a dev build): dialog offers Continue (unsupported) or "Install release firmware 1.4.0" (a downgrade, flow A with differential write).

## 4. Branching, CI and release flow

**Branches:**
- `main` = released state (live on Pages), protected: PR only, required checks.
- Work happens on feature branches, with one PR per change.

**CI (`.github/workflows/ci.yml`, on every PR and push):**

| Job | Runs | Notes |
|---|---|---|
| `go` | `go vet`, `go test -race ./...`, 60 s fuzz per target, `ctpbench vectors check` | ubuntu runner, Go version from go.mod |
| `c-host` | firmware core: lint (cppcheck, clang-tidy), unit tests with ASan/UBSan in **C11 and gnu23**, fuzz-short, coverage, `fw-host-conf` (conformance vs PTY firmware) | ubuntu runner |
| `js` | `node --test test/js/` (codec, filter, semver, romflash framing vs shared vectors) | Node runtime only, no npm packages |
| `firmware` | builds `firmware/` and `test_apps/idf_api_smoke` inside **`container: docker.io/espressif/idf:<idf-version.txt>`** | GitHub Actions runs the job container itself. Same pinned image as local Podman builds (doc 07 §7.1) |
| `bundle-check` | rebuilds firmware from source and compares with committed `fw/*.bin` **byte-for-byte** (reproducible build, doc 07 M0) and checks `fw/manifest.json` hashes/offsets | Guarantees the live page ships firmware that matches the source in the same commit |
| `semver-check` | if `testvectors/`, doc 03 or protocol code changed since the last tag, the version in `fw/manifest.json` must bump at least MINOR; if protocol `ver` changed, MAJOR | doc 03 §11 rules |
| `web-check` | static checks: all `index.html`/JS references are relative and exist; precache list matches files; no external URLs | Prevents broken Pages deploys |

Hardware-in-the-loop (L5–L8, doc 07 §6) runs locally, or on an optional **self-hosted runner** with a dongle attached (`runs-on: [self-hosted, ctk-hil]`, job `hil`: `ctpbench fw ensure --yes && ctpbench conformance`). It never runs on GitHub-hosted runners.

**Release (`make release VERSION=X.Y.Z`, run by the developer on a branch):**
1. Build firmware in the pinned container (reproducible) → write `fw/` (bins + manifest with version, build_id, offsets from `flasher_args.json`, sha256).
2. Run all host-side tests.
3. Commit "release X.Y.Z" → PR → CI (incl. `bundle-check`) → merge to `main`.
   - Merging **is** the web release: Pages redeploys within minutes.
4. `release.yml` (on push to `main` when `fw/manifest.json` version changed):
   - create tag `vX.Y.Z`;
   - create a GitHub Release with the `fw/` files, a merged image, and **cross-compiled ctpbench binaries** (linux amd64/arm64/arm, darwin amd64/arm64, windows amd64; `CGO_ENABLED=0`);
   - after the tag exists, `go install github.com/<owner>/capturetonkeli/cmd/ctpbench@latest` resolves to it, with the same firmware embedded.

**Consistency guarantee:** web UI, `fw/` bundle, ctpbench (embedded `fw/`) and the tag all come from **one commit**. The only way a mismatch reaches a user is an old dongle (or a dev build), and that case is handled by the prompt.

## 5. Developer and agent workflow summary (CLAUDE.md, root)
```markdown
## Repo facts
- index.html at repo root is the live GitHub Pages site. main = production. Never push to main directly.
- All web paths relative (site lives under /capturetonkeli/). No external URLs, no npm, no build step.
- fw/ is generated by `make release`; never edit by hand. CI verifies it byte-for-byte.
- Go module root = repo root. Library in ctp/, CLI in cmd/ctpbench.
- Firmware: see firmware/ (doc 07). Pinned IDF in firmware/idf-version.txt. Do not change the pin without asking.
- Local web test: `make serve` (python3 -m http.server on localhost; Web Serial works on localhost) or the demo mode.
```

## 6. Changes this makes to other docs
| Doc | Change |
|---|---|
| 04 | §0: hosting = GitHub Pages sub-path, relative URLs, `.nojekyll`; §2 file list at repo root; SW policy → doc 11 §3; versioned URLs dropped |
| 05 | module root = repo root; package paths `ctp/…`, `cmd/ctpbench`, `fw` (embed) |
| 06 | repo conventions → doc 11 §1; `go install …@latest` |
| 07 | §2 path prefix `firmware/`; CI uses the Actions job container with the same pinned image; release artifacts → doc 11 §4 |
| 08 | D3 resolved (single latest URL); B1 resolved by doc 10 |
| 09 | "always latest" pairing: page's own `fw/manifest.json` is the expected firmware |
