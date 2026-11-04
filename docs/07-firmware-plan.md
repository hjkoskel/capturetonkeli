# 07 – ESP32-C5 firmware: architecture, coding rules, development stages and test plan (v1 draft, 2026-11-04)

Firmware for the XIAO ESP32-C5 capture dongle.
Implements CTP v1 (doc 03), the filter semantics (doc 02 §2 / doc 04 App. A) and the radio rules (doc 02 §4–5).
It is verified by the Go reference library, simulator and test bench (docs 05–06).

---

## 1. Key decisions

| Topic | Decision |
|---|---|
| Language | **C11** (`-std=c11`). No C++. The design needs nothing C++ offers, and plain C keeps one mental model across host and target and makes static analysis simpler |
| Framework | ESP-IDF **v6.0.2** (pinned; bugfix release of v6.0, which Espressif supports until Sept 2028; matches the developer's native install). The pin lives in `firmware/idf-version.txt` (content: `v6.0.2`) and selects the container image tag `docker.io/espressif/idf:v6.0.2`. Bumping the pin is a deliberate change: re-run M4 radio checks + full conformance |
| Build environment | **Native ESP-IDF** when installed and matching the pinned version (fastest, the default on a dev PC). **Podman** container (rootless) as the fallback and in CI. Docker works as a drop-in via `CONTAINER=docker`. Selection is automatic (§7.1) |
| Structure | **Portable core + thin ports.** All protocol and filter logic is pure C with zero ESP-IDF includes, so it compiles and is tested on a PC. Only small adapter files touch ESP-IDF |
| Memory | **No heap use after init.** All buffers are static, with sizes checked by `_Static_assert` |
| Concurrency | Fixed set of FreeRTOS tasks, created once. Data passes through one lock-free SPSC ring and one FreeRTOS queue. Config is published by double-buffer swap |
| Robustness | Every byte from USB is untrusted. Bounds-checked readers. Fail-safe defaults (capture off at boot). Crash-loop safe mode. Reset reason is reported to the host |
| Testability | Same core runs in three places: (1) host unit tests, (2) **host firmware executable on a PTY** that `ctpbench conformance` talks to, (3) the real dongle |

---

## 2. Source layout

The firmware lives in `firmware/` of the single project repository (repo layout, hosting and release: doc 11). ESP-IDF v6 API details and pitfalls: doc 10.
```
firmware/
  idf-version.txt
  idf-api.txt                    every IDF symbol/enum/Kconfig we use → generates the API smoke app (doc 10 §5)
  CMakeLists.txt                 ESP-IDF project
  sdkconfig.defaults             baseline per doc 10 §4 (only defaults committed; generated sdkconfig git-ignored)
  main/
    app_main.c                   composition root: init order, task creation (no logic)
  components/
    ctp_core/                    PORTABLE – no ESP-IDF headers allowed (CI greps for it)
      include/ctp/*.h
      src/
        cobs.c          COBS encode/decode (streaming decoder)
        crc32.c         CRC-32 IEEE (table); port may override with ROM esp_rom_crc32_le
        rd.c            bounds-checked little-endian reader/writer ("cursor" with sticky error flag)
        frame.c         header + CRC + COBS → frames; resync; oversize
        wire.c          every message body encode/decode, forward-compat rules
        filter.c        SET_FILTER decode/validate, compiled form, match()
        ring.c          SPSC lock-free byte ring (C11 atomics) for captured frames
        proto.c         protocol engine: dispatcher, req_id dedup cache (4), cfg_gen,
                        host watchdog, HELLO gating, response building
        config.c        config model, validation, apply sequencing through the port interface
        stats.c         counters (atomic where written from callback)
        capture.c       promiscuous-callback body: meta extraction, filter, snaplen, push to ring
      CMakeLists.txt    builds as IDF component OR plain static lib (if(ESP_PLATFORM))
    ctp_port_esp/                ESP-IDF adapters – the only code touching IDF APIs
      usb_usj.c       usb_serial_jtag driver: read/write with timeouts, connection state
      radio_esp.c     esp_wifi init NULL mode, promisc, band/channel/bw/country, driver masks
      sys_esp.c       time (esp_timer), reboot (normal/download), reset reason, no-init RAM assert record, app descriptor (version/build id)
      cb_esp.c        promiscuous rx callback → capture.c
  host/                          PC build (CMake, gcc/clang)
    CMakeLists.txt
    port_host/
      usb_pty.c       PTY or stdin/stdout transport
      radio_fake.c    fake radio: synthetic traffic or replay from pcapng; honours channel/country tables
      sys_host.c      monotonic clock (optionally virtual), reboot = re-exec/reinit
    tests/            unit tests (Unity, vendored single file, MIT) + generated vector headers
    fuzz/             libFuzzer targets (clang -fsanitize=fuzzer,address,undefined)
    fw_host_main.c    builds "ctp_fw_host": the real core + host ports, a dongle on a PTY
  test_apps/
    target_unit/      ESP-IDF Unity test app: ring atomics, port adapters, timing on real RISC-V
    idf_api_smoke/    compile/link check of every IDF API we use + 5 s hardware check (doc 10 §5)
  Makefile            one-word entry points for humans and agents (§7)
```

**Port interface** (`ctp/port.h`), the only seam between core and platform:
```c
typedef struct {
    /* link */
    int32_t  (*usb_write)(const uint8_t *buf, uint32_t len, uint32_t timeout_ms);  /* returns bytes written or <0 */
    int32_t  (*usb_read)(uint8_t *buf, uint32_t cap, uint32_t timeout_ms);
    /* radio */
    ctp_err_t (*radio_set_country)(const ctp_country_t *c);
    ctp_err_t (*radio_set_channel)(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied_bw);
    ctp_err_t (*radio_set_driver_mask)(uint32_t type_mask, uint32_t ctrl_mask);
    ctp_err_t (*radio_promisc)(bool on);
    /* system */
    uint64_t (*now_us)(void);
    void     (*reboot)(ctp_reboot_mode_t mode);          /* does not return */
    int32_t  (*last_esp_err)(void);                      /* for RSP_ERR detail */
} ctp_port_t;
```
Errors from the port carry the raw platform code (`esp_err_t` on target), which goes straight into `RSP_ERR.esp_err`.

---

## 3. Runtime architecture

```
 Wi-Fi driver task                    ctp_link task (prio high)            ctp_ctrl task (prio mid)
 ─────────────────                    ─────────────────────────            ───────────────────────
 promisc cb → capture.c               loop:                                 waits on req queue:
  · read active filter idx (atomic)    · usb_read (≤1 ms timeout)            · proto.c handles request
  · match + snaplen                    · frame decoder → requests ──queue──► · config.c applies via port
  · reserve/commit in SPSC ring        · drain ctrl-out queue FIRST          · builds response → ctrl-out queue
  · or drop_ring++ (never blocks)      · then drain packet ring:             · 1 s timer: EVT_STATS, host watchdog
                                         encode PKT frames back-to-back
                                         into 2 KB TX buffer → usb_write
```
- **Exactly one writer per piece of state:**
  - the ring has one producer (callback) and one consumer (link);
  - config is written only by `ctp_ctrl`;
  - stats counters written from the callback are `atomic_uint_fast32_t` with relaxed ordering.
- **Filter/snaplen publication (no locks in the callback):**
  - two compiled filter slots;
  - `ctp_ctrl` writes the inactive slot, then `atomic_store(&active, slot)` (release);
  - the callback does `atomic_load` (acquire) once per frame and bumps `cb_epoch` on exit;
  - before overwriting a slot again, `ctp_ctrl` waits until `cb_epoch` has advanced (bounded wait ≤ 10 ms, else retry). No torn reads are possible.
- **Response priority** (doc 03 §5): the link task always empties the ctrl-out queue before taking packets from the ring.
- **Batching without latency:** the link task fills one 2 KB TX buffer with as many complete frames as are ready and writes immediately. It never waits to fill.
- **USB back-pressure:** the link task never blocks more than 5 ms in `usb_write`. If the host isn't reading, frames stay in the ring until it fills, then `drop_ring` counts. The Wi-Fi task is never stalled.
- **Host watchdog:** no valid frame from the host for 2 s → capture off, packet ring flushed, stop emitting. HELLO is required again before streaming.

### Memory budget (C5: 384 KB HP SRAM)
| Item | Size |
|---|---|
| Packet ring | 96 KB (tunable, `_Static_assert` ≤ 128 KB) |
| RX frame buffer + COBS scratch | 2 × 4.2 KB |
| TX buffer | 4.5 KB (`CTP_TX_BUF` 4608: holds one max-size PKT frame). M3: packet batches are capped at 512 B (`CTP_TX_BATCH`) and the USJ driver TX buffer is 1 KB, so ≤ ~1.5 KB sits ahead of a control response (PING p99 3 ms at full load). A batch that cannot be written for 250 ms is dropped so a stalled host never wedges the link |
| Filter slots (compiled) | 2 × ~2.4 KB |
| Ctrl queues | out: 8 × 208 B (largest body EVT_LOG 201 B); requests: 4 × 2.4 KB (max SET_FILTER 2328 B). Full request queue → the link stops reading USB (back-pressure), so bursts are never dropped |
| Task stacks | link 4 KB, ctrl 4 KB (verified by high-water mark reporting) |
| Wi-Fi driver | per IDF defaults, RX buffers tuned in M4 |

---

## 4. "Fool-proof" coding rules (enforced)

**Language subset** (MISRA-C:2012-inspired, pragmatic):
1. Fixed-width integer types only. No implicit narrowing: `-Wconversion -Wsign-conversion` are errors.
2. No `malloc/free` outside init. No VLAs, no `alloca`, no recursion, no `goto` except a single `cleanup:` pattern.
3. **Never cast wire bytes to structs.** All wire access goes through `rd.c` cursors (`rd_u16le(&c)`). The cursor has a sticky `err` flag; you check it once at the end, and reading past the end yields zeros and sets `err`. This one rule removes the whole class of alignment, endianness and overread bugs.
4. Every length field is validated against both the protocol limit and the remaining buffer before use. Every array index is range-checked or provably bounded by a `_Static_assert`.
5. Every function that can fail returns `ctp_err_t`. Results go via out-parameters. `[[nodiscard]]`-style enforcement via `__attribute__((warn_unused_result))`.
6. `switch` over enums lists every case plus `default` with an error path (`-Wswitch-enum`).
7. `const` everywhere possible. `static` for everything not in a header. One module = one `.c` + one `.h`.
8. No floating point in core code.
9. Asserts (`CTP_ASSERT`) stay **on** in release builds. A failure records file/line in no-init RAM (`__NOINIT_ATTR`, survives software reset; doc 10 §3) and reboots. The next HELLO/EVT_RESET reports it to the host (`reset_reason = ASSERT`, detail = `file:line`).
10. The callback path (`capture.c`) has a documented worst-case budget (target ≤ 5 µs/frame). No logging, no blocking, no flash access (IRAM-safe where required).

**Compiler / analysis gates (all `-Werror`):**
- gcc: `-Wall -Wextra -Wconversion -Wsign-conversion -Wshadow -Wformat=2 -Wcast-align=strict -Wstrict-prototypes -Wmissing-prototypes -Wswitch-enum -Wundef -Wnull-dereference -fanalyzer` (host)
- clang host build: `-fsanitize=address,undefined` for all tests; `-fsanitize=fuzzer` for fuzz targets
- `cppcheck --enable=all --inline-suppr` and `clang-tidy` (bugprone-*, cert-*, clang-analyzer-*) with zero findings
- CI grep: no `#include "esp_` / `freertos/` inside `ctp_core`; no `malloc` in core
- Formatting: `clang-format` checked in CI (style file in repo)

**Fail-safe behaviour:**
- **Boot:** capture off, country "01" in RAM, no streaming until HELLO. The host pushes the full config every session.
- **Zero flash writes at runtime** (doc 09): Wi-Fi initialised with `nvs_enable = 0` / `WIFI_STORAGE_RAM`; country never persisted; PHY calibration data not stored (full calibration at boot; boot time measured in M4). Flash is written only by firmware updates. A debug counter of flash writes is exposed in stats, and conformance asserts it stays 0.
- **Crash-loop guard:** 3 abnormal resets within 60 s → **safe mode**. Radio stays off; only HELLO, PING, GET_CONFIG, GET_STATS and REBOOT are answered; everything else → `RSP_ERR(ERR_SAFE_MODE)`. The host shows why. A normal REBOOT or power cycle clears it.
- **Task watchdog** on link and ctrl tasks. Brownout detector on.
- **Unknown request type** → `RSP_ERR(ERR_UNKNOWN_TYPE)`, never silence (conformance CTP-REQ).

---

## 5. Development stages

Each stage ends with a **gate**: concrete commands whose exit code 0 means done. Stages M1–M2 need no hardware.

### M0 – Skeleton & CI (½–1 day)
- Repo layout per §2, build-environment selection (native IDF / Podman, §7.1), Makefile targets, CLAUDE.md firmware section.
- Empty core compiles on host and target; Unity vendored; generated test-vector headers wired up.
- **IDF API smoke build** (doc 10 §5) passes against the pinned IDF. Decide v6.0.2 vs v6.0.3 here.
- GitHub repo skeleton per doc 11: root `index.html` placeholder live on Pages, `.nojekyll`, CI workflow green.
- **Gate:** `make ci` (host build + IDF build + static checks) green, and `make fw` produces byte-identical app binaries with `BUILD_ENV=native` and `BUILD_ENV=podman` (same IDF tag, `CONFIG_APP_REPRODUCIBLE_BUILD=y` in sdkconfig.defaults, which maps absolute paths such as those in `CTP_ASSERT`'s `__FILE__`). If they differ, document why before moving on. This is a cheap way to prove "works on my machine" equals "works in CI".

### M1 – Portable core: codec layer (host only)
- `rd.c`, `cobs.c`, `crc32.c`, `frame.c`, `wire.c`, `filter.c`.
- Unit tests from the shared vectors, which `go generate` emits as C headers (`tv_cobs.h`, `tv_frames.h`, `tv_filter_match.h`, …). This needs no JSON parser in C.
- Fuzz targets: frame decoder, every message decoder, filter decoder (each ≥ 10 min locally, 60 s in CI).
- **Differential test:** `ctpbench`'s Go `filter.Match` and the C `filter_match()` run on 100k random (filter, frame, meta) cases generated with a seed. A mismatch prints a minimal repro vector that is added to `filter_match.json`.
- **Gate:** `make host-test fuzz-short coverage` → 100% line / ≥ 95% branch coverage on codec+filter, zero sanitizer reports.

### M2 – Protocol engine on PC ("firmware in the loop without hardware")
- `proto.c`, `config.c`, `stats.c`, `ring.c`, `capture.c` + host ports (`usb_pty.c`, `radio_fake.c`, `sys_host.c`) → `ctp_fw_host` executable.
- `radio_fake` emits synthetic 802.11 traffic (or replays a pcapng), applies country tables like ESP-IDF, and models the USB rate limit.
- **The real firmware core is now a dongle on a PTY.** `ctpbench --port /dev/pts/N conformance` runs the full protocol suite against it.
- **Gate:** `make fw-host && ctpbench --port $(make -s fw-host-pty) conformance --skip 'CTP-(FLT-RF|LIVE-RTT)'` passes. The same suite passes against the Go simulator, which proves the suite itself.

### M3 – Hardware bring-up: USB link (first hardware stage)
- Throughput firmware variant: link task streams counter frames as fast as possible; measure with `ctpbench bench throughput`.
- Answer the doc 01 open risks:
  - DTR/RTS behaviour on open for Linux/macOS/Windows/Android (does any combination reset the C5?) → fix `SafeOpenSignals`;
  - real sustained USJ throughput;
  - `usb_write` timeout behaviour;
  - 50 ms stall when no host.
- Tune TX buffer size and ring size from measurements.
- **ROM flasher bring-up:** `ctpbench fw flash-bare` and `fw update` (Go `romflash`) and the web flows A/B/C against real hardware. Verify the USB-JTAG DTR/RTS reset-into-bootloader sequence on Linux/macOS/Windows/Android, re-enumeration handling, MD5 per sector, and the differential sector count.
- **Gate:** `ctpbench bench throughput --save-baseline usb` recorded; doc 01/03 updated with measured numbers; `wait-device` works after flash; `ctpbench fw flash-bare --yes` brings a factory-fresh XIAO C5 to a HELLO-answering state; `fw update` on an unchanged image writes 0 sectors.

*M3 status (2026-11-04):* throughput baseline recorded (`make fw-synth flash-synth` + `ctpbench bench throughput --save-baseline usb`); flashing via REBOOT 1 verified (doc 10 §3); Go `romflash` + `ctpbench fw flash/update/ensure/flash-bare` work on hardware (unchanged image: 0 sectors written in ~4.5 s; full image ~13 s; an interrupted write recovers with a plain retry). Open: macOS/Windows/Android, `fw flash-bare` on a factory-fresh chip, web flows.

### M4 – Radio bring-up
- `radio_esp.c`: NULL-mode init, promiscuous on/off, band switching, channel/bw, country (validated + manual), driver masks.
- Determine:
  - the accepted channel map per country (`ctpbench bench channel-map`, answers doc 02 §7.1);
  - channel-switch latency (`bench channel-switch`);
  - whether the C5 frame-type misreport exists in the pinned IDF (doc 02 §7.2);
  - `rx_ctrl` field mapping (rate, sig_mode, noise floor, timestamp source).
- Measure the callback rate on a busy channel; verify the callback stays within its time budget (cycle counter).
- **Gate:** channel-map report committed as `docs/measurements/channel-map-<idf>.json`; on-target unit tests (`test_apps/target_unit`) pass.

*M4 status (2026-11-04):* channel map committed (`docs/measurements/channel-map-v6.0.2.json`), channel-switch latency 2/3 ms, no frame-type misreport in v6.0.2 (doc 02 §7.2), callback budget met: avg 3.6 µs (850 cycles) after removing a 64-bit division and cursor overhead from the hot path (hot path also placed in IRAM via `ctp_core/linker.lf`; that alone did not change the average). rx_ctrl: FCS absent → `no_fcs`; legacy rate mapping implemented. Open: MCS extraction from `he_siga1`, on-target unit tests (`test_apps/target_unit`).

### M5 – Integration on hardware
- Full pipeline: callback → filter → ring → link task → host. HELLO gating, watchdog, cfg_gen, EVT_STATS, reboot modes, safe mode, RAM-only country (flash-write counter stays 0).
- **Gate:** `ctpbench conformance` passes on hardware (except injector tests); web UI (doc 04) connects and captures on desktop Chrome, Firefox and Android Chrome.

### M6 – Performance & robustness
- **Targets:**
  - PING RTT p99 < 5 ms under full load (desktop);
  - zero host-side loss at nominal USB rate;
  - the device drops only via `drop_ring` (counted), never corrupts.
- `ctpbench fuzz --duration 30m` with the device alive throughout; `ctpbench soak --duration 24h` with no resets, no stack high-water growth, no counter anomalies.
- Optional injector end-to-end (`ctpbench inject`, doc 06 §5) for filter correctness on real RF.
- **Gate:** baselines saved; soak report clean; `--compare-baseline` in CI for any later performance change.

### M7 – Release v1
- **Semantic versioning** per doc 03 §11. The version comes from the git tag `vMAJOR.MINOR.PATCH` via `git describe`. The build fails if the tag isn't strict semver. Untagged builds get the `dev` flag; uncommitted changes set `dirty`.
  - The version is written into `PROJECT_VER` (→ `esp_app_desc_t.version`) and the packed `fw_version`.
  - `fw_build_id` = first 8 bytes of the app SHA-256, which IDF already embeds in `esp_app_desc_t.app_elf_sha256`. Read it at runtime; no post-processing needed.
- **Bump discipline:**
  - MAJOR when protocol `ver` changes or something is removed;
  - MINOR for any new message, field, cap, default or capture-visible behaviour;
  - PATCH otherwise.
  - CI check: if `testvectors/` or doc 03 changed since the last tag, the next tag must bump at least MINOR.
- **Release artifacts:**
  - `manifest.json` (doc 04 §11.1 format; offsets taken from `build/flasher_args.json`) + `bootloader.bin`, `partition-table.bin`, `ctk-X.Y.Z.bin`, plus a merged image;
  - IDF version, test reports.
  - `make release VERSION=X.Y.Z` writes the bundle to the repo-root `fw/`. That directory is served by GitHub Pages and embedded in ctpbench (package `fw`). Merging the release PR to `main` publishes the web UI; `release.yml` then tags and creates the GitHub Release (doc 11 §4). **UI, CLI and firmware are released together from one commit.**
- **Gate:** release checklist = all previous gates re-run on the tagged commit.

---

## 6. Test pyramid summary

| Level | Where | What | Tooling | When |
|---|---|---|---|---|
| L0 Static | PC | warnings-as-errors, cppcheck, clang-tidy, fanalyzer, layering greps | make lint | every commit |
| L1 Unit | PC | every core module, shared vectors, edge cases | Unity + ASan/UBSan + gcov | every commit |
| L2 Fuzz | PC | decoders, filter compiler | libFuzzer | 60 s per target in CI, long runs nightly |
| L3 Differential | PC | C filter vs Go reference, C codec vs Go codec on random input | ctpbench + host lib | every commit |
| L4 Firmware-in-loop | PC | whole core via PTY, protocol conformance | ctp_fw_host + ctpbench | every commit |
| L5 On-target unit | dongle | atomics/ring on RISC-V, port adapters, timing budgets | IDF Unity test app | per PR touching ports |
| L6 HIL conformance | dongle | full suite + benches vs baselines | ctpbench | per PR (local/agent), nightly on a bench machine |
| L7 RF end-to-end | 2 dongles | injector scenarios | ctpbench inject | before release |
| L8 Soak | dongle | 24 h stability | ctpbench soak | before release |

---

## 7. Daily development loop (human or Claude Code)

Makefile entry points (all non-interactive, exit code = result):
```
make host-test        # L0+L1 (+sanitizers), seconds
make fuzz-short       # L2, 60 s per target
make diff-test        # L3, needs Go toolchain
make fw-host-conf     # L4: builds ctp_fw_host, starts it on a PTY, runs ctpbench conformance, stops it
make fw               # idf.py build: native IDF if it matches idf-version.txt, else the pinned image via Podman
make flash            # idf.py flash to the dongle found by `ctpbench list`, then `ctpbench wait-device`
make hil              # L6: ctpbench conformance + benches --compare-baseline
make ci               # what CI runs: lint host-test fuzz-short diff-test fw-host-conf fw
```

### 7.1 Build environment: native IDF or Podman

**Selection (`BUILD_ENV ?= auto`):**
- `auto`:
  - If `$IDF_PATH` is set (i.e. `export.sh` was sourced) and `idf.py --version` matches `firmware/idf-version.txt` → **native**.
  - If IDF is present but the version differs → **error with a hint**: "local IDF v5.4.1 ≠ pinned v6.0.2: run `make fw BUILD_ENV=podman`, or check out the pinned IDF". It never silently builds with the wrong IDF, because radio behaviour is IDF-version-specific (doc 02 §7.2).
  - Otherwise → **podman**.
- `BUILD_ENV=native` / `BUILD_ENV=podman` force one path. `IDF_VERSION_CHECK=0` lets an experienced user knowingly try another IDF (the build then stamps `idf-mismatch` into the firmware version string, so test reports show it).
- The Makefile prints the chosen environment as its first line (`build-env: native (IDF v6.0.2 at ~/esp/esp-idf)`), so agents and humans can see which one ran.

**Native path:**
- Uses your installed toolchain directly: `idf.py -C firmware build`, `idf.py flash`, `idf.py monitor`. Incremental builds are fastest here.
- Host-side targets (`host-test`, `fuzz-short`, `fw-host-conf`, lint) never need IDF; they use the system gcc/clang.
- For agents, sourcing `export.sh` in every shell is error-prone. The Makefile therefore runs native IDF commands via `bash -c '. $(IDF_PATH)/export.sh >/dev/null && idf.py …'` when `idf.py` isn't already on `PATH`. `IDF_PATH` can be set once in a git-ignored `local.mk`.

**Podman path** (CI, clean machines, or reproducing a CI result exactly):

- `CONTAINER ?= podman` in the Makefile. The image is `docker.io/espressif/idf:$(cat firmware/idf-version.txt)`. Always use the fully qualified name, because Podman doesn't assume Docker Hub.
- **Build** (no device access needed):
  ```
  podman run --rm --userns=keep-id -e IDF_GIT_SAFE_DIR=/project \
         -v "$PWD":/project:Z -w /project/firmware \
         docker.io/espressif/idf:$IDF_TAG idf.py build
  ```
  - `--userns=keep-id` makes files in `build/` owned by your user, not a subuid. That avoids the classic "root-owned build dir" problem from Docker.
  - `:Z` relabels the volume for SELinux hosts (Fedora/RHEL); it is harmless elsewhere.
- **Flashing: from the host, not the container (default).** The build output is on the host anyway. `make flash` runs `esptool.py` or `idf.py flash` from a host install (or `pipx run esptool`). Rootless containers and USB serial devices are a frequent source of permission trouble, and keeping the device out of the container keeps `make flash` + `ctpbench wait-device` simple.
- **Optional in-container flashing** (`make flash-container`):
  ```
  podman run --rm --userns=keep-id --group-add keep-groups \
         --device /dev/ttyACM0 -v "$PWD":/project:Z -w /project/firmware \
         docker.io/espressif/idf:$IDF_TAG idf.py -p /dev/ttyACM0 flash
  ```
  - `--group-add keep-groups` passes the host user's `dialout`/`uucp` membership into the rootless container; it requires the crun runtime.
  - The device node changes after reboot/download mode, so pass `--device` for the path that `ctpbench list` reports at that moment.
- **Host-only targets** (`host-test`, `fuzz-short`, `fw-host-conf`, lint) use the local gcc/clang. They can also run in a small dev image (`firmware/host/Containerfile`: gcc, clang, cppcheck, clang-tidy, Go) so every developer and agent uses identical tool versions: `make host-test CONTAINER_HOST=1`.
- **Caching:** with `--userns=keep-id` the build runs as your UID, not root, so don't mount caches at `/root/...`. Use `-v ctk-idf-cache:/cache:U -e CCACHE_DIR=/cache/ccache -e IDF_CCACHE_ENABLE=1 -e HOME=/cache/home`. The `:U` flag chowns the named volume to the container user.
- **Image tag check (M0):** confirm `docker.io/espressif/idf:v6.0.2` exists (`podman pull`). If Espressif hasn't published that patch tag, build a slim local image from IDF's own `tools/docker` Dockerfile with `--build-arg IDF_CLONE_BRANCH_OR_TAG=v6.0.2 --build-arg IDF_CLONE_SHALLOW=1 --build-arg IDF_INSTALL_TARGETS=esp32c5`. Podman builds it with `podman build`, and it is smaller anyway (C5 toolchain only). Never fall back to the floating `v6.0` / `release-v6.0` tags, which move.
- **git inside the container:** the firmware version string comes from `git describe`. Pass `-e IDF_GIT_SAFE_DIR=/project`, which the IDF image supports, so git doesn't refuse the mounted repo as "dubious ownership".
- **M0 gate addition:** verify on the pinned image that a keep-id (non-root) build works end to end (Espressif documents running the image as `-u $UID -e HOME=/tmp`, so this is expected to work). If the image needs root for anything, fall back to running as root with `--userns=keep-id` dropped and a final `podman unshare chown` of `build/`. The Makefile hides whichever variant is chosen.
- **macOS/Windows:** `podman machine` works for builds. Flashing is always from the host there, since USB passthrough into the Podman VM is not practical.
- **CI (GitHub Actions)** runs the firmware job inside the same pinned image as a job container (`container: docker.io/espressif/idf:<pin>`), invoking the same Makefile targets with `BUILD_ENV=native`. Local builds use Podman with the identical image, so CI and local results match (doc 11 §4).

Suggested **CLAUDE.md** firmware section:
```markdown
## Firmware rules
- Core logic lives in firmware/components/ctp_core and must not include ESP-IDF headers.
- Change core → `make host-test fw-host-conf` must pass before touching hardware.
- Hardware loop: `make fw flash hil`. The build uses native IDF if it matches firmware/idf-version.txt, else Podman; flashing always runs on the host. On failure read bench-out/latest/report.json only.
- Never change firmware/idf-version.txt to match the local IDF; ask the user instead.
- Never cast wire bytes to structs; use rd.c cursors. No malloc after init. Warnings are errors.
- IDF v6 pitfalls (doc 10): WIFI_BW20/WIFI_BW40 (not *_HT20/HT40), never WIFI_BAND_MODE_AUTO, esp_wifi_init once, no assert(), include freertos/FreeRTOS.h explicitly, never touch GPIO13/14 or the USB PHY. New IDF symbol → add it to firmware/idf-api.txt.
- New protocol behaviour: update doc 03 + Go reference (ctp/, repo root) + testvectors FIRST, then C, then web.
```

**Order of work for any protocol change** (keeps the three implementations in lockstep):
1. Change the spec (doc 03).
2. Change the Go reference library and simulator, then `go generate` the vectors.
3. C core passes the new vectors on host (L1–L4).
4. Hardware HIL (L6).
5. Web UI (against Go sim / demo device first).

---

## 8. Risks tracked for the firmware

| Risk | Mitigation / stage |
|---|---|
| DTR/RTS open sequence resets C5 on some OS | measure M3; host libs set safe signals; document per OS |
| USJ throughput lower than ~0.5 MB/s | M3 measurement; increase snaplen guidance in UI; consider PKT header compaction (v2) |
| Promisc callback too slow at 100% channel load | M4 cycle budget; compiled filter (precomputed masks, early FC-byte reject) |
| IDF frame-type bug / driver regressions | we don't rely on driver type masks; pinned IDF; channel-map + conformance re-run on every IDF bump |
| Flash wear | no runtime flash writes at all (country/PHY in RAM); updates write only changed sectors and only on version change |
| Assert in field reboots mid-capture | reset reason + file:line reported; safe mode stops crash loops; host auto re-push |
