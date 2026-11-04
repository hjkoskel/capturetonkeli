# 06 – Test bench program specification "ctpbench" (v1 draft, 2026-11-04)

A command-line test bench for developing and verifying the ESP32-C5 firmware, built on the Go library (doc 05).
It is designed first for **agentic development** (Claude Code CLI and similar): an agent edits firmware, flashes it, runs `ctpbench`, reads machine-readable results and iterates, with no human at the keyboard.
Humans get the same commands with readable text output.

---

## 0. Design principles for agent use

1. **Non-interactive always.** No prompts, no pagers, no TTY requirement, no curses. Anything that would ask needs a flag (`--yes`) or fails with a clear error.
2. **Bounded runtime.** Every command terminates on its own:
   - global `--timeout` (default 30 s; streaming commands require `--duration` or `--count`);
   - a hung device never hangs the agent;
   - Ctrl-C / SIGTERM exit cleanly and still emit the final JSON summary.
3. **Machine-readable output.**
   - `--json` → exactly one JSON document on stdout.
   - Streaming commands use `--jsonl`: one JSON object per line, ending with a `{"type":"summary",…}` line.
   - Human logs go to **stderr** only.
   - If stdout is not a TTY, `--json` is the default (overridable with `--text`).
4. **Stable exit codes** (§3) so a shell or agent can branch without parsing.
5. **Errors explain themselves.** Every error object carries `code` (stable string), `message`, and `hint` (what to try next), e.g. `{"code":"PORT_BUSY","message":"/dev/ttyACM0 is open by another process (pid 4121 idf.py monitor)","hint":"stop the monitor or pass --port of another dongle"}`.
6. **Idempotent and composable.** Setting the same config twice is fine. Commands chain in shell scripts. Nothing persists on the host except explicit artifacts.
7. **Self-describing.**
   - `ctpbench help --json` lists every command, flag and output schema.
   - `ctpbench schema <command>` prints the JSON Schema of that command's output.
   - `--help` text is short and example-first.
8. **Artifacts, not scrollback.** Long outputs (captures, logs, reports) go to files under `--out-dir` (default `./bench-out/<run-id>/`). The JSON result lists their paths, so an agent reads only what it needs.
9. **Deterministic when possible.** `--seed` for simulator and fuzz runs. Every report records the seed, firmware version, library version and host OS.
10. **Single binary, stdlib only.** `go install github.com/<owner>/capturetonkeli/cmd/ctpbench@latest` (always the latest release, with that release's firmware embedded via package `fw`), prebuilt binaries from GitHub Releases, or `go build ./cmd/ctpbench` in the repo. No config files required; an optional `ctpbench.json` in the working directory sets defaults (port, out-dir).

## 1. Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--port PATH\|auto\|sim` | `auto` | dongle path; `auto` = only dongle found (error if 0 or >1); `sim` = in-process simulator (doc 05 §8) |
| `--json` / `--jsonl` / `--text` | auto | output format |
| `--timeout DUR` | 30s | hard cap for the whole command |
| `--out-dir DIR` | `./bench-out/<run-id>` | artifact directory (created on demand) |
| `--seed N` | random, reported | simulator / fuzz determinism |
| `--sim-faults SPEC` | – | with `--port sim`: e.g. `drop=0.001,corrupt=0.0005,stall=3s@10s,reboot@20s` |
| `-v` / `-vv` | – | stderr verbosity (protocol trace at `-vv`) |
| `--trace FILE` | – | write raw TX/RX frames (hex + decoded) as JSONL for post-mortem |
| `--allow-fw-mismatch` | off | connect even if firmware MAJOR.MINOR ≠ ctpbench's bundled/expected version (doc 03 §11). The result JSON then carries `"unsupported_fw": true` |
| `--fw-manifest PATH` | embedded | manifest defining the expected firmware version (default: the `fw` package embedded in the binary = the repo's `fw/manifest.json` at build time; in a dev tree `--fw-manifest firmware/build/manifest.json` targets a local build) |

## 2. Commands

### Discovery & session
| Command | Does | JSON result (key fields) |
|---|---|---|
| `list` | enumerate serial ports and dongles | `ports[]{path,vid,pid,serial,is_dongle}` |
| `doctor` | environment check: port permissions (Linux `dialout`/udev rule), port busy and by whom, DTR/RTS safe-open, HELLO, protocol version match | `checks[]{name,ok,detail,hint}` |
| `info` | HELLO + GET_CONFIG | `device{fw_version,proto_ver,mac,caps[],max_frame,max_rules}`, `config{…}` |
| `ping [--count N] [--interval D]` | RTT measurement | `rtt_ms{min,median,p95,p99,max}`, `lost` |

**Firmware version handling (non-interactive by design):** every command that connects first runs the doc 03 §11 check.
- **MAJOR or MINOR mismatch:** exit code **9**, with a JSON error
  `{"code":"FW_MISMATCH","device":"1.2.4","expected":"1.3.0","device_build_id":"3fa9…","hint":"run `ctpbench fw update` or pass --allow-fw-mismatch"}`.
- **Protocol mismatch:** exit code 5, hint: `ctpbench fw update`.
- **PATCH difference:** proceeds, with an `info` entry in the result.
- In a TTY with `--text`, the same message is printed in human form. ctpbench still **never prompts** (§0); the "prompt" is the error plus the two explicit next commands.

### Firmware
| Command | Does | JSON result |
|---|---|---|
| `fw info` | HELLO only (allowed even on mismatch): version, flags, build_id; compares with the manifest | `device{version,flags,build_id}`, `expected{…}`, `compatible`, `exact_build_match` |
| `fw update [--yes]` | flow A: differential update of a CTP dongle to the manifest version (REBOOT 1 → ROM → changed sectors → MD5 verify → reset → HELLO check) | `sectors_written`, `sectors_skipped`, `duration_ms`, `new_version` |
| `fw flash-bare [--erase-all] --yes` | flow B: full flash of a bare/unknown ESP32-C5 (DTR/RTS reset into ROM, or `--manual-boot` waits for the user to plug in with BOOT held); refuses non-C5 chips | same + `chip`, `flash_size` |
| `fw flash --manifest PATH \| --image FILE --offset ADDR [--allow-version FILE_VER] --yes` | flow C: flash arbitrary build (dev/downgrade). Refuses MAJOR.MINOR ≠ expected unless `--allow-version` | same |
| `fw ensure [--yes]` | idempotent: does nothing if compatible and the exact build_id matches, otherwise `fw update`. **The agent's one-liner before any HIL run** | `action: "none" \| "updated"` |

`--yes` is required for anything that writes flash. Without it the command prints the plan (parts, sectors to write) and exits 0. That makes it a dry run, useful for agents.

### Configuration (each returns the device readback)
| Command | Maps to |
|---|---|
| `config get` | GET_CONFIG |
| `config set --file cfg.json` | apply a complete desired config in doc 03 §7 order; reports per-step result |
| `country set CC [--manual --schan N --nchan N]` | SET_COUNTRY |
| `channel set CH [--bw 20\|40+\|40-]` | SET_CHANNEL |
| `filter set --file f.json \| --preset NAME[:ARG] … \| --clear` | SET_FILTER (+ CRC verify) |
| `filter check --file f.json` | validate + encode locally; prints body hex + CRC (no device needed) |
| `snaplen set N` | SET_SNAPLEN |
| `drivermask set --type MASK --ctrl MASK` | SET_DRIVER_MASK |
| `capture on\|off` | CAPTURE |
| `stats [--reset]` | GET_STATS |
| `reboot [--download] --yes` | REBOOT (waits for re-enumeration in normal mode, reports `reset_reason`) |

`cfg.json` is the same object as the web UI settings "desired config" (doc 04 §14), so configs are shareable.

### Capture & observation
| Command | Does |
|---|---|
| `capture run --duration D \| --count N [--pcapng FILE] [--jsonl-packets] [--display-filter EXPR]` | configure (optional `--config`), capture, write pcapng to out-dir; summary with counts, drops (device/host), rates, per-type histogram, top BSSIDs |
| `monitor --duration D` | JSONL stream of everything except packet bodies: events, stats, gaps, link state, cfg changes. Agents use it to watch firmware behaviour |
| `verify-pcap FILE` | parse a pcapng with the library reader, check structure/radiotap; returns packet count and anomalies |
| `raw send TYPE BODYHEX [--req-id N]` | send an arbitrary frame and print the response(s). For negative tests and protocol work |
| `raw stream --duration D` | hex dump of raw decoded frames (incl. CRC-failed ones flagged) |

### Testing
| Command | Does |
|---|---|
| `conformance [--run REGEX] [--skip REGEX] [--list]` | runs the protocol conformance suite (§4) against the device (or sim) |
| `bench throughput [--snaplen N] [--duration D]` | max sustained PKT/s and B/s before `drop_ring` > 0; latency packet→host |
| `bench rtt-under-load [--duration D]` | PING RTT distribution while capturing at full rate (doc 03 test 3) |
| `bench channel-switch [--channels LIST] [--repeat N]` | SET_CHANNEL latency; within-band vs cross-band |
| `bench channel-map [--country CC]…` | walks every 2.4/5 GHz channel under each country mode and records accepted/rejected + error. This answers doc 02 §7 open item 1 |
| `fuzz --duration D [--seed N]` | sends malformed frames (bad CRC, oversize, garbage, truncated bodies, unknown types, version mismatch) interleaved with valid PINGs; device must stay alive and answer valid requests |
| `soak --duration D [--report-every D]` | long run: capture + periodic config changes + reconnects; detects leaks (device RAM in EVT_LOG/stats), stalls, resets |
| `inject …` | see §5 (requires injector dongle) |

### Simulator & tooling
| Command | Does |
|---|---|
| `sim serve --pty \| --tcp ADDR` | runs the Go simulator on a pseudo-terminal (Linux/macOS, prints the PTY path) or TCP. Lets the **web UI** and other clients be developed without hardware, via a tiny local bridge page or `socat` |
| `vectors check` | verify that `testvectors/` match the current library (detects drift between implementations) |
| `report merge DIR…` | merge several JSON reports into one summary (for CI) |

## 3. Exit codes

| Code | Meaning |
|---|---|
| 0 | success; all checks passed |
| 1 | test failures (conformance/bench thresholds/verify) – details in JSON |
| 2 | usage error (bad flags/arguments) |
| 3 | no device / port not found / ambiguous `auto` |
| 4 | port busy or permission denied |
| 5 | protocol error: handshake failed, version mismatch |
| 6 | device returned RSP_ERR for a requested operation |
| 7 | timeout / link dead |
| 8 | internal error (bug in ctpbench) – includes stack trace in `--out-dir/crash.txt` |
| 9 | firmware version mismatch (MAJOR.MINOR) and `--allow-fw-mismatch` not given |
| 10 | flashing failed (dongle left in download mode; safe to retry) |

## 4. Conformance suite

Each test case has a stable ID, a one-line purpose, and a reference to the spec clause it verifies. Every result reports **expected vs actual** and a **hint** pointing to the likely firmware area.
```json
{"id":"CTP-REQ-003","title":"duplicate req_id is answered from cache, not re-applied",
 "spec":"doc03 §5","status":"fail","duration_ms":212,
 "expected":"cfg_gen unchanged after duplicate SET_CHANNEL","actual":"cfg_gen 7 -> 8",
 "hint":"firmware request dedup cache (last 4 req_ids) not consulted before apply",
 "trace":"bench-out/2026-11-04T10-12-03/CTP-REQ-003.trace.jsonl"}
```

Test groups (initial list):
| Group | Covers |
|---|---|
| `CTP-FRM-*` | COBS/CRC framing, resync after garbage, oversize rejection, empty frames ignored, CRC-bad frame never answered |
| `CTP-HDR-*` | ver field, seq monotonic per direction, req_id echo, cfg_gen increments only on applied change, flags bit1 |
| `CTP-HELLO-*` | nonce echo, caps/max_* sanity, no packets before HELLO, capture off after boot |
| `CTP-REQ-*` | one response per request, dedup cache, RSP_ERR shape, unknown type → RSP_ERR (not silence), forward-compat longer bodies accepted |
| `CTP-CFG-*` | each SET returns readback; country table enforcement; band switch; bw downgrade reported; snaplen 0/N; filter CRC equality; driver mask readback |
| `CTP-FLT-*` | firmware filter decisions equal `filter.Match` on reference traffic (needs injector, §5; otherwise only encoding/CRC tests run) |
| `CTP-PKT-*` | PKT field sanity: cap_len ≤ snaplen, orig_len ≥ cap_len, truncated flag, rx_seq continuity, channel field == configured |
| `CTP-LIVE-*` | PING RTT bound under load, 2 s watchdog stops capture, ctrl frames not starved by packets, EVT_STATS cadence 1 s ± 100 ms |
| `CTP-RST-*` | REBOOT → EVT_RESET; re-handshake; desired config re-push restores identical CONFIG |
| `CTP-ROB-*` | fuzz survival, host disappears mid-stream (close without CAPTURE 0) → device recovers on next open |

Outputs:
- `report.json` (full);
- `junit.xml` for CI;
- one-line summary on stderr: `conformance: 57 passed, 2 failed, 3 skipped (needs injector) in 41.2s`.

`--list --json` prints the suite metadata without a device. An agent can then select tests with `--run`.

## 5. Optional injector (end-to-end RF tests)

For true end-to-end verification of capture and filtering, the bench supports a **second XIAO ESP32-C5 running test-only "injector" firmware**. It transmits known frames with `esp_wifi_80211_tx()`.
- Same CTP framing. The injector advertises `caps.bit31 = INJECTOR` and adds one test-only message: `0x40 INJECT{u8 channel, u16 count, u16 interval_us, u8 frame[]}`, response RSP_OK after the burst. **Never present in production firmware.**
- `ctpbench inject --injector PORT --capture PORT --scenario FILE`:
  1. configures both on the same channel;
  2. the injector sends a scripted sequence of frames (each containing a unique sequence number + marker in the body);
  3. the bench checks on the capture dongle which frames arrived, that filtering matched `filter.Match` exactly, timestamps are monotonic, and no frame was corrupted.
  - Reports per-frame loss (separating RF loss from dongle/host loss using rx_seq/seq) and packet latency (inject → host receive).
- ⚠️ This transmits. Use minimum TX power, short bursts, ideally in a shielded box or with attenuators, and only in a lawful setup. The bench refuses to run `inject` without `--i-understand-this-transmits`.

## 6. Agentic workflow integration (Claude Code)

### Repository conventions the bench assumes
Normative layout: doc 11 §1. In short:
```
index.html, css/, js/, sw.js   web UI at repo root (GitHub Pages)
fw/                  current release firmware bundle (+ embed.go)
go.mod, ctp/, cmd/ctpbench/, conformance/   Go module at repo root
firmware/            ESP-IDF project (idf.py build / flash)
testvectors/         shared vectors
bench-out/           artifacts (git-ignored)
CLAUDE.md            agent instructions (below + doc 11 §5)
```

### Suggested `CLAUDE.md` section
```markdown
## Firmware dev loop
1. Build:   idf.py -C firmware build
2. Flash:   make flash   (dev builds: idf.py flash; or `ctpbench fw flash --manifest firmware/build/manifest.json --yes`)
            (if the port is busy: `ctpbench doctor`)
   Release/HIL machines: `ctpbench fw ensure --yes` before testing.
   Exit code 9 = firmware MAJOR.MINOR mismatch: never pass --allow-fw-mismatch on your own; update or ask the user.
3. Smoke:   ctpbench info --json
4. Verify:  ctpbench conformance --json --out-dir bench-out/last   (exit 0 = pass)
5. On failure read only bench-out/last/report.json; follow each failed case's "hint" and its .trace.jsonl.
6. Performance changes: ctpbench bench throughput --json and compare with bench-out/baseline/throughput.json.
Without hardware use --port sim (the simulator is the reference behaviour).
Never run `ctpbench inject` unless the user asks.
```

### Agent-friendly details
- **Port hand-off with `idf.py flash/monitor`:**
  - the bench opens the port exclusively and reports who holds it if busy (Linux: scan `/proc/*/fd`);
  - `ctpbench wait-device [--timeout 10s]` blocks until the dongle re-enumerates after flashing and answers HELLO. This removes sleep-guessing from agent scripts.
- **Baselines:** `bench … --save-baseline NAME` and `--compare-baseline NAME [--tolerance 10%]` give exit code 1 on regression with a per-metric diff in JSON.
- **Small outputs by default:** summaries stay under ~4 KB of JSON; details go to artifact files. `--fields a,b,c` trims output further.
- **Reproduction line:** every report includes `repro`, the exact command line (with seed) that re-runs just the failed cases.
- **Time-stamped run IDs** and a `bench-out/latest` symlink so agents never guess paths.

## 7. Non-goals (v1)
- GUI. The web UI covers interactive use.
- Windows PTY simulator (`sim serve --tcp` works on all OSes).

## 8. Implementation notes
- `cmd/ctpbench` uses only stdlib: `flag` with a small subcommand dispatcher, `encoding/json`, `encoding/xml` (JUnit), `os/signal`.
- Every command is a function `func(ctx, env) (result any, err error)`. One output layer renders JSON/JSONL/text and maps errors to exit codes, so commands never print directly.
- Conformance cases are table-driven Go code in `conformance/` at the repo root (importable). The same cases run in `go test` against the simulator, which proves the suite itself is correct before it judges firmware.
