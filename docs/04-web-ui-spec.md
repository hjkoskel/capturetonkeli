# 04 – Web UI specification (v1 draft, 2026-11-04)

Browser UI for the capturetonkeli dongle. Implements **every** feature of the USB link protocol in doc 03 (CTP v1).
References: doc 01 (transports), doc 02 (filters, radio, regulatory), doc 03 (protocol).

---

## 0. Hard constraints

1. **Vanilla only:** HTML5 + CSS + JavaScript (ES2022) as the browser executes it.
   - No frameworks, no libraries, no npm, no bundler, no transpiler, no build step, no CDN, no web fonts loaded from third parties.
   - No TypeScript, no JSX, no CSS preprocessors. JSDoc comments for types are allowed.
   - Only standard Web Platform APIs.
2. **Hosting: GitHub Pages from the repository root** (`main`, folder `/`). The site lives under a sub-path (`https://<owner>.github.io/capturetonkeli/`), so **every URL is relative** and `.nojekyll` is present (doc 11 §2). `http://localhost` (`make serve`) for development. Web Serial and WebUSB require a secure context, so `file://` is not supported. No server-side code, no network calls after load except the update check of `fw/manifest.json` and `version.json` at startup (§19; both same origin).
3. **Offline-capable and always latest:** a Service Worker precaches the app. Navigation and `fw/manifest.json` are network-first, and a new release shows a "Reload" banner, never an automatic reload during capture (doc 11 §3). There are no per-version URLs; users always run the latest page.
4. **No telemetry, no external requests.** Captured data never leaves the device except through files the user saves.
5. Files are plain ES modules (`<script type="module">`) plus one dedicated Worker. Each JS file has one responsibility (§2). Total size target < 200 KB uncompressed.

## 1. Supported platforms

| Platform | Browser | Transport | Status |
|---|---|---|---|
| Windows / macOS / Linux / ChromeOS | Chrome, Edge, Opera (Chromium ≥ 89) | Web Serial | Supported |
| Windows / macOS / Linux | Firefox ≥ 151 | Web Serial | Supported |
| Android (USB OTG) | Chrome | WebUSB + built-in CDC-ACM shim | Supported |
| iOS / iPadOS, Safari (any OS), Android WebView | – | – | Not supported → show explanation page |

**Transport selection (automatic, overridable in Settings):**
- Not Android: `navigator.serial` → Web Serial; else `navigator.usb` → WebUSB CDC shim.
- Android: Web Serial **once it has shown a USB port on this phone** (a granted port with a USB vendor id at load, or the start page's *Test Web Serial*: the dongle appears in the chooser); otherwise WebUSB. Chrome on Android has Web Serial since 148, but wired ports need Android's serial API, which exists only on some devices (docs/01); older builds list only Bluetooth ports. The result is stored per Chrome major version (`androidSerial`), so an update tests again. Logic: js/capabilities.js `chooseTransport`.
- Neither → the "unsupported browser" panel listing the supported browsers.

Settings → Transport overrides this (auto / Web Serial / WebUSB).

---

## 2. Architecture

All paths below are relative to the **repository root**, which is also the Pages site root (full repo layout: doc 11 §1).
```
index.html            layout skeleton, all UI markup, <noscript> message
.nojekyll             serve files as-is on GitHub Pages
css/app.css           single stylesheet, CSS custom properties for theming
js/main.js            bootstraps, wires modules, feature detection
js/state.js           single app state object + EventTarget-based change notifications
js/transport-serial.js   Web Serial backend   ─┐ common interface:
js/transport-webusb.js   WebUSB CDC-ACM shim  ─┘ open(), close(), write(u8), onChunk(cb), onError(cb)
js/protocol.js        CTP client: req_id allocation, request/response matching, timeouts/retry,
                      HELLO/PING lifecycle, config push, cfg_gen tracking
js/worker.js          Dedicated Worker: COBS decode, CRC-32, header parse, PKT/EVT decode,
                      seq-gap accounting, packet store, display-filter evaluation, pcapng encoding
js/codec.js           shared pure functions (COBS enc/dec, CRC-32, struct read/write) – imported by worker and main
js/dot11.js           minimal 802.11 header decoder (type/subtype names, addresses, SSID IE, seq/frag, flags)
js/pcapng.js          pcapng + radiotap writer
js/ui/*.js            one module per panel (connection, radio, filter, capture, packets, detail, stats, log, settings)
js/demo-device.js     simulated dongle implementing CTP v1 (for development and demo mode, §13)
js/romflash.js        ESP ROM download-protocol flasher (SLIP, SYNC, chip detect, FLASH_*, SPI_FLASH_MD5), §11.1
js/semver.js          parse/compare/compatibility rule (doc 03 §11)
js/wire.js            message bodies + shared constants (App. B); js/regdom.js is generated from ctp/regdom
js/filter.js          filter model, presets, encoder, evaluator (shared by UI, worker and demo device)
js/display-filter.js  display filter language (§8), compiled in the worker
js/errors.js          every failure → {title, text, actions, detail} shown inline/banner and logged (§12)
js/app.js             connection state machine, worker bridge, recording; js/settings.js, js/log.js
fw/manifest.json      bundled firmware manifest (version, build_id, parts + offsets + sha256), §11.1.
                      Also the single source of the page's own version (no separate version constant)
fw/*.bin              bundled firmware images matching this page version (generated by `make release`)
sw.js                 Service Worker at site root (update policy: doc 11 §3)
manifest.webmanifest  PWA manifest (installable on Android/desktop)
```

### Data flow
```
transport ──chunks(ArrayBuffer, transferred)──► worker
worker ──► decoded responses/events (postMessage, small) ──► protocol.js ──► state ──► UI
worker ──► packet store (ring) ; UI pulls "view windows" at ≤30 fps
UI ──► protocol.js ──► codec (COBS+CRC) ──► transport.write
```
- If `navigator.serial` / `navigator.usb` is available inside the Worker, the transport runs in the Worker too. Otherwise the main thread only pumps raw chunks into it.
- The main thread **never** parses packets. That keeps the read loop responsive and avoids Web Serial `BufferOverrunError`.
- Packet store: preallocated ring of N packets (default 200 000, configurable 10k–2M), backed by one large `ArrayBuffer` for bytes + typed-array columns for metadata. No per-packet JS objects.

### Performance requirements
| Metric | Requirement |
|---|---|
| Sustained ingest | ≥ 15 000 PKT/s and ≥ 1.2 MB/s without host overrun on a mid-range laptop; ≥ 5 000 PKT/s on a mid-range Android phone |
| Packet-to-screen latency | ≤ 50 ms (p95) from chunk arrival to visible row while auto-scroll is on |
| Command feedback | UI shows applied readback within 1 RTT. Spinner if > 150 ms |
| Rendering | ≤ 30 fps, requestAnimationFrame-driven, virtualized list (only visible rows in DOM) |
| Memory | Bounded by packet-store size. No growth during long captures |

---

## 3. Screen layout

Desktop (≥ 900 px wide): three regions.
```
┌───────────────────────────── top bar ──────────────────────────────┐
│ [Connect] ● link state · RTT · fw/proto · [Capture ▶/■] [Record ●] │
├──────────── side panel (tabs) ─────┬──────── packet list ──────────┤
│ Radio | Filter | Stats | Device    │ virtualized table             │
│ | Log | Settings                   │                               │
│                                    ├──────── detail pane ──────────┤
│                                    │ decoded fields + hex dump     │
└────────────────────────────────────┴───────────────────────────────┘
```
Mobile (< 900 px):
- Top bar stays sticky with Connect, link dot and Capture/Record buttons.
- The side-panel tabs become a bottom tab bar (Packets, Radio, Filter, Stats, More).
- The detail pane is a full-screen sheet opened by tapping a row.
- Touch targets ≥ 44 px.

Theme: light and dark via `prefers-color-scheme` + manual override. Colours from CSS variables only.

---

## 4. Connection & session (protocol: transport open, HELLO, PING, EVT_RESET, versioning, caps)

### Connect button
States: `disconnected → choosing → opening → handshaking → connected`. On a lost link it goes to `reconnecting`.

On click:
1. Request the port/device. Filter on VID 0x303A, PID 0x1001; on WebUSB also class 0x02.
2. Open:
   - Web Serial: `open({baudRate: 115200, bufferSize: 1 MiB})`. Baud is ignored by USJ but required by the API. **Do not call `setSignals`** (M3 finding, doc 01 risk #1: the OS default DTR=1/RTS=1 does not reset the app, and toggling RTS wedged the ROM loader).
   - WebUSB: select config, claim the data interface, SET_LINE_CODING, SET_CONTROL_LINE_STATE (DTR|RTS, the same state an OS driver sets), then start ≥ 8 parallel IN transfers.
3. Wait for the first `0x00` delimiter, then send **HELLO** `{host_nonce = random u32, host_proto_ver = 1}`.
4. Validate the response:
   - Nonce echo must match.
   - `proto_ver ≠ 1` → protocol mismatch: the dialog in §4.1 opens with only "Flash bundled firmware" and "Cancel" (talking to the device is impossible, so there is no "continue").
   - **Firmware version check** (doc 03 §11): compare `fw_version` with the bundled `fw/manifest.json` version. Different MAJOR or MINOR → the §4.1 dialog. Steps 5+ wait until the user decides.
   - Store `fw_version`, `fw_flags`, `fw_build_id`, `mac`, `caps`, `max_frame`, `max_rules`, `device_time_us`. The latter defines the offset device↔host clock (§9 timestamps).
5. **GET_CONFIG**, then **push desired config** (§5–§7) in the order SET_COUNTRY → SET_CHANNEL → SET_SNAPLEN → SET_DRIVER_MASK → SET_FILTER. If "auto-start capture" is enabled → CAPTURE 1.

Previously granted ports: on page load, `navigator.serial.getPorts()` / `navigator.usb.getDevices()`. If exactly one known dongle is present, show "Reconnect to dongle AA:BB:…" as a one-tap button. No auto-connect without a user gesture on the first visit.

### 4.1 Firmware version mismatch dialog

Shown when MAJOR or MINOR differ (or the protocol version differs). Modal `<dialog>`, keyboard accessible:

```
Firmware version mismatch
  Dongle:  1.2.4  (build 3fa9c1d2…)
  Page:    1.3.0  (bundled firmware build 77b0e412…)
This page is tested only with firmware 1.3.x.

[ Update dongle to 1.3.0 ]   (recommended, default button – about 20 s, only changed flash sectors are written)
[ Continue with 1.2.4 ]      (unsupported – some features may misbehave)
[ Cancel ]
☐ Don't ask again for this dongle and this firmware version
```
- **Update** → §11.1 update flow, then reconnect and continue at step 5.
- **Continue** → the session runs in **"unsupported version" mode**:
  - a permanent amber banner "Firmware 1.2.4 ≠ page 1.3.x (unsupported)";
  - the mode is written into recordings (pcapng SHB comment) and logs;
  - features are still gated by `caps`;
  - unknown/extra fields are ignored per doc 03 §11.
- **Don't ask again:** stored per dongle MAC + firmware version in `localStorage` (only applies to "Continue"). Removed when either version changes.
- **PATCH-only difference** → no dialog; info line in the Device tab ("Firmware 1.3.2, page bundles 1.3.0 – compatible"), plus an "Update to bundled" button there.
- `fw_flags.dirty` / `dev` / `idf_mismatch` → amber badge next to the version in the top bar. These never trigger the dialog on their own.
- Protocol mismatch → same dialog, but without "Continue".
- Users always run the latest page (doc 11), so the normal case is "dongle older than page" and Update is the default. If the dongle is *newer* (developer dev build), the dialog wording becomes "Install release firmware 1.3.0 (downgrade)" next to "Continue".

### Link status indicator (top bar)
| Colour | Meaning |
|---|---|
| Green | Connected; last frame < 1 s ago |
| Yellow | PING retries in progress, or any host-side loss in the last 5 s |
| Red | Link dead (3 PING timeouts) or transport error |
| Grey | Disconnected |

Next to it:
- RTT: median of the last 20 PINGs, plus p95 on hover.
- fw version, protocol version, device MAC (short).

### Keepalive
- PING every 500 ms with `host_time_ms`. Each reply updates RTT.
- 3 consecutive timeouts (200 ms each, per doc 03 retry rule) → link dead → `reconnecting`:
  - close the transport;
  - reopen with the same port object every 1 s for 30 s;
  - then give up and show a "Reconnect" button.
- Page `visibilitychange` to hidden: keep the read loop and PINGs running. Only DOM rendering pauses, so background captures continue on desktop. On Android, if the OS suspends the tab, the device's 2 s watchdog stops capture. When the page is visible again the UI reconnects and shows a banner: "Capture was paused by the device while this tab was in the background (n s gap)".

### Resync events
- **EVT_RESET** received → log entry + banner "Dongle restarted (reason: …)". Run steps 3–5 again automatically and set the `resumed` marker in the packet list and recording (§10).
- **Unexpected `cfg_gen`** (changed without a pending request) → GET_CONFIG. If it differs from desired → re-push and log "Config drift corrected".
- **Host overrun / transport error** → host counter `overruns++`. Reopen the stream, wait for a delimiter, re-handshake.

### Capability gating (`caps`)
- bit0 5 GHz absent → the 5 GHz band is hidden.
- bit1 HT40 absent → bandwidth selector is fixed to HT20.
- Unknown bits are ignored.
- Every capability-gated control shows a tooltip explaining why it is unavailable.

### Disconnect
Button in the top bar menu. Sends CAPTURE 0, waits for its response (≤ 200 ms), closes the transport, then flushes and closes any open recording.

---

## 5. Radio panel (SET_CHANNEL, SET_COUNTRY, readback, RSP_ERR)

### Country / regulatory
- Explanation text (one paragraph, collapsible): "The dongle only receives, so no channels are restricted for transmission. ESP-IDF still uses the country table to decide which channels it will tune to; pick the table that unlocks the channels you need." (doc 02 §5)
- **Mode** selector:
  - *Validated country code*: dropdown of the supported codes ("01" world safe, AT … US). Default = the browser locale's region if it is in the list, else "01". Sends `SET_COUNTRY{cc, mode 0}`.
  - *Manual range (advanced, only shown if `caps.bit2 MANUAL_COUNTRY`)*: 2.4 GHz start channel (1–14) and count (1–14). Sends `SET_COUNTRY{cc "XX", mode 1, schan, nchan}`. Warning icon: "Unvalidated table. Receive-only use."
- **Apply** button, enabled only when the value differs from the readback.
  - Note under it: "Kept in dongle RAM only and re-sent on every connect (no flash writes)."

### Channel
- **Band** segmented control: 2.4 GHz / 5 GHz (5 GHz only if caps bit0).
- **Channel grid:**
  - 2.4 GHz: buttons 1–14.
  - 5 GHz: 36…177 grouped by UNII band, with DFS channels marked ("DFS: receive OK").
  - Each button shows the centre frequency on hover.
  - Channels the current country table rejects stay clickable but appear dimmed, using a host-side copy of the table (best effort). The device readback is the authority.
- **Bandwidth:** HT20 / HT40+ / HT40− (HT40 only if caps bit1 and the channel allows it).
- Selecting a channel sends **SET_CHANNEL** immediately; the request is optimistic and the UI shows a pending state.
  - On `RSP_OK` the UI applies the readback. If the readback differs from what was asked (e.g. bandwidth downgraded), it shows an inline notice "Requested HT40+, dongle applied HT20".
  - On `RSP_ERR` the selection reverts and the error is shown inline under the grid, mapped per §12, e.g. "Channel 13 not allowed by country 01 → switch country table?" with a one-click jump to the country control.
- Keyboard: ←/→ previous/next channel within the band (sends SET_CHANNEL). Useful for manual sweeping.
- **Current radio readback box:** band, channel, frequency MHz, bandwidth, country + mode, noise floor (from the latest EVT_STATS), cfg_gen.

---

## 6. Filter panel (SET_FILTER, rules_crc32 sync)

### Rule editor
- A **rule set** = list of rules (OR). A **rule** = list of terms (AND). This mirrors doc 02 §2.
- Limits: rules ≤ `max_rules` (from HELLO, ≤ 16); terms per rule ≤ 4; term length 1–16 bytes; offset 0–4095. Filters are meant as short fingerprints near the start of the frame, so these limits are ample. The editor enforces them, and violations block Apply with an inline message.
- **Term row:**
  - offset (decimal or `0x` hex input);
  - **value** hex field (bytes separated by spaces, `:` or `-`; MAC addresses can be pasted directly);
  - **mask** hex field, same length; default all `FF`. A "?" in a nibble of value sets that mask nibble to 0, e.g. `8?` = value 0x80, mask 0xF0;
  - **negate** toggle;
  - delete button.
  - Length is derived from the value; value and mask length mismatch → red outline.
  - A live "offset ruler" shows which 802.11 header field the term covers (FC, Duration, Addr1, Addr2, Addr3, SeqCtl, Addr4/QoS…), via `dot11.js` field map.
- **Rule header:** name (host-side only), enable/disable checkbox (disabled rules are not sent), duplicate, delete, move up/down.
- **Global conditions** (meta terms, apply to all frames before rules):
  - minimum RSSI (dBm, slider −100…0, off);
  - length min / max (bytes, off);
  - "Drop frames with bad FCS" (default **on**).
- **Empty rule set = pass everything.** The panel shows "No rules: all frames pass" in that case.

### Presets (insert as a new rule; parameters prompted inline)
- Beacons only
- Probe requests
- Probe responses
- All management
- All control
- All data
- Drop beacons (negated)
- From MAC… (addr2)
- To MAC… (addr1)
- BSSID… (addr3)
- Deauth/disassoc
- EAPOL (two rules: non-QoS and QoS offset)
- Custom byte pattern…

### Apply & sync
- **Apply** encodes the set (Appendix A), sends SET_FILTER and shows pending → applied.
- The host computes CRC-32 over the exact encoded body and compares it with `rules_crc32` from the CONFIG readback:
  - Equal → badge "✓ In sync (crc 1A2B3C4D)".
  - Different (e.g. after a device reboot) → badge "⚠ Device filter differs" + button "Push mine" / "Clear on device".
- Unsaved edits → badge "Edited – not applied"; capture keeps running with the old filter.
- **Import/Export** rule set as JSON (`{version:1, rules:[…], global:{…}}`) via file picker / download. Named rule sets are saved in `localStorage`.

### Test against captured packet
In the packet detail pane, a button "Make rule from this packet" creates a rule matching the selected packet's addr2 + FC type/subtype. Each rule also has a "Would match?" indicator for the selected packet, evaluated host-side with the same algorithm as the firmware (implemented once in `codec.js`, shared).

---

## 7. Capture panel elements (SET_SNAPLEN, SET_DRIVER_MASK, CAPTURE)

These sit in the Filter tab under "Capture options".

### Snaplen
- Number input + presets: **Full (0)**, 32 (MAC header), 64, 128, 256, custom.
- Value 0 is shown as "unlimited".
- Applied immediately (debounced 300 ms) with readback.
- Hint with an estimate from current stats: "At the current 2 400 frames/s, full frames ≈ 1.9 MB/s – exceeds USB (~1 MB/s); expect drops." Uses the average `orig_len` of recent packets.

### Driver pre-filter (advanced, collapsed by default)
- Checkboxes for the ESP-IDF promiscuous type mask bits (MGMT, CTRL, DATA, MISC, DATA_MPDU, DATA_AMPDU, FCSFAIL).
- Checkboxes for the control-subtype mask bits (e.g. RTS, CTS, ACK, BlockAck, PS-Poll), sent as `SET_DRIVER_MASK{type_mask, ctrl_mask}`.
- Bit values come from a single constant table in `protocol.js` that mirrors the ESP-IDF headers of the firmware's pinned IDF version.
- Default: all types + all control subtypes.
- Warning text: "On ESP32-C5 the driver may mis-classify frame types (doc 02 §1). Prefer user filter rules."

### Capture toggle (top bar ▶/■)
- Sends CAPTURE 1/0 and reflects `capture_on` from the readback.
- Disabled while disconnected.
- Setting: "Auto-start capture on connect" (default on).

---

## 8. Packet list & detail (PKT)

### List (virtualized table)
| Column | Source |
|---|---|
| # | host sequential index |
| Time | relative to capture start (default) / absolute host time / device µs. Selectable in the header menu |
| Ch | PKT.channel (+ "40" badge if HT40 flag) |
| RSSI | dBm, with small bar |
| Rate | legacy Mb/s or "MCSn" + sig_mode (11bg / HT / VHT-HE) |
| Len | orig_len; shows "cap/orig" when truncated |
| Type | decoded type/subtype name (dot11.js), e.g. "Beacon", "QoS Data", "ACK" |
| Src / Dst / BSSID | addr2 / addr1 / addr3, if captured. Optional vendor-OUI hint off by default (no bundled database > 50 KB) |
| Info | SSID for beacons/probes; seq/frag; flags (Retry, Protected, More Data); "FCS ERR" in red |

- **Markers inside the list** (full-width separator rows, not packets): "Config changed: ch 36 → 44 (cfg_gen 7)", "Dongle restarted", "Gap: 132 frames dropped on dongle (rx_seq)", "Gap: link frames lost on host (seq)", "Reconnected".
  - These come from `cfg_gen` changes, EVT_RESET, `rx_seq` gaps and header `seq` gaps.
- **Auto-scroll** toggle; it turns off when the user scrolls up, with a "Jump to live (n new)" pill.
- **Freeze view** (Space key): stops list updates; capture and recording continue.
- **Display filter** (host-side, does not change the device):
  - text box with a small expression grammar: `type==beacon`, `ssid~"home"`, `rssi>-60`, `src==aa:bb:cc:dd:ee:ff`, `ch==6`, `len>200`, `fcs_err`, combined with `and`/`or`/`not`/parentheses;
  - evaluated in the worker;
  - shows "showing X of Y";
  - invalid expression → red outline + message.
- Clear list (does not touch the recording).
- Colour rules: mgmt / ctrl / data tinted subtly; FCS errors red; truncated frames get an italic length.

### Detail pane
- Metadata block: all PKT fields, including device timestamp µs, noise floor, SNR (rssi − noise), flags decoded, cfg_gen active, rx_seq.
- Decoded 802.11 header tree: Frame Control bits, Duration, addresses, Seq/Frag, QoS, HT control if present. For mgmt frames, the IE list: SSID, Supported Rates, DS Param (channel), RSN summary, HT/VHT/HE capability presence, Vendor IEs (OUI shown). Decoding beyond the captured bytes shows "[truncated]".
- **Hex dump**: offset column, 16 bytes per row, ASCII column. Hovering a decoded field highlights its bytes and vice versa.
- Buttons: "Make rule from this packet", "Copy hex", "Copy as Wireshark hex-dump text" (`0000  xx xx …` format, importable with Wireshark "Import from Hex Dump").

---

## 9. Statistics panel (EVT_STATS, GET_STATS, host counters)

Updated on every EVT_STATS (1 s). Device counters, shown as totals and per-second rates (delta / Δdevice_time):
- rx_total, rx_passed (and pass ratio %), drop_ring, drop_fcs, tx_frames, tx_bytes (as kB/s and % of a 1 MB/s nominal USB budget);
- host_rx_crc_err / host_rx_oversize (seen by the device on commands);
- **ring_high_water** as a horizontal gauge (green < 50%, yellow < 90%, red ≥ 90%);
- channel, noise floor.

Host counters (computed in the worker):
- frames received, header seq gaps (frames lost on host), COBS/CRC errors, oversize frames, unknown types, transport overruns, reconnects;
- bytes/s in, parse time per second (ms), packet-store fill.

**Loss diagnosis line**, one sentence derived from the counters, e.g.:
- "Dongle dropping 18% (USB saturated) → lower snaplen or tighten filters."
- "Host losing frames → close other tabs / reduce packet store / use desktop."

Sparkline per key rate (last 120 s) drawn on one `<canvas>`.

Buttons:
- **Refresh now** → GET_STATS{reset_after_read 0}.
- **Reset counters** → GET_STATS{reset_after_read 1} + reset host counters, after a confirm dialog.

---

## 10. Recording / export (pcapng + radiotap)

- **Record ●** toggle in the top bar. The packet list is only a view; the recording is independent of it.
- Format: **pcapng**:
  - SHB, then one IDB with LINKTYPE 127 (IEEE 802.11 + radiotap), `snaplen` = current snaplen or 65535, `if_tsresol` = µs.
  - One EPB per PKT. The radiotap header contains TSFT (device µs), Flags (FCS-at-end when the FCS is present in captured bytes, bad-FCS), Rate or MCS, Channel (freq + band flags), dBm Antenna Signal, dBm Antenna Noise.
  - `orig_len` → EPB original length; truncated frames stay correct.
  - **Comments:** when cfg_gen changes, on dongle restart and on gaps, the next EPB gets an `opt_comment`, e.g. "cfg_gen 8: ch 44 HT20, country FI, filter crc …" or "gap: 132 dropped on dongle". Wireshark shows these.
  - SHB options: `shb_userappl` = "capturetonkeli web v1", hardware = "XIAO ESP32-C5 fw x.y.z".
- **Storage backends** (auto, best available):
  1. **File System Access API** (`showSaveFilePicker`, desktop Chromium): stream directly to the user-chosen file, flushing every 1 s or 1 MB.
  2. **OPFS** (`navigator.storage.getDirectory()`, Firefox desktop and Android Chrome): stream into an OPFS file. On stop → offer "Download" (anchor with Blob from the OPFS file). The list of previous OPFS recordings has download/delete buttons and storage usage (`navigator.storage.estimate()`).
  - The encoding runs in the worker. Recording never blocks the read loop.
- Options:
  - file name template (default `capture_YYYYMMDD-HHMMSS_ch{ch}.pcapng`);
  - split file every N MB / N minutes;
  - record only packets matching the display filter (off by default).
- **Export current buffer**: write the packets currently in the store (all, or only those matching the display filter) as pcapng, even if Record was off.

---

## 11. Device tab (REBOOT, HELLO info, GET_CONFIG)

- Info table from HELLO: firmware version, protocol version, MAC, caps decoded, max_frame, max_rules, device uptime (from device_time_us, live), host↔device clock offset.
- Full **CONFIG readback** view (all fields, raw + decoded, cfg_gen) + **Re-read config** button (GET_CONFIG).
- **Re-push my config** button. This is the same code path as reconnect step 5.
- **Reboot dongle** (REBOOT mode 0), after a confirm dialog. The UI expects the link drop, then auto-reconnects and re-handshakes.
- **Reboot to download mode** (REBOOT mode 1), after a confirm dialog. Mostly for external tools; the flashing flows below enter download mode themselves.

### 11.1 Firmware: update, bare-chip flashing, flash from file

The flasher (`js/romflash.js`) talks to the C5's **mask-ROM download mode**. It is implemented from Espressif's published serial protocol, in vanilla JS, with no third-party stub binaries (ROM loader only).

**Bundled firmware:** `fw/manifest.json`, produced by the firmware release build (doc 07 §M7):
```json
{ "version": "1.3.0", "build_id": "77b0e412a8c3d901", "protocol_ver": 1, "idf": "v6.0.2", "chip": "esp32c5",
  "flash": { "mode": "dio", "freq": "80m", "size": "8MB" },
  "parts": [ { "name": "bootloader", "offset": "0x2000", "file": "bootloader.bin", "sha256": "…" },
             { "name": "partition-table", "offset": "0x8000", "file": "partition-table.bin", "sha256": "…" },
             { "name": "app", "offset": "0x10000", "file": "ctk-1.3.0.bin", "sha256": "…" } ] }
```
Offsets come from the build's `flasher_args.json`. They are never hard-coded in the UI. (The ESP32-C5 bootloader offset 0x2000 is confirmed by Espressif's boot docs; the other values are illustrative.)

**Entering download mode** (in order of preference):
1. Running CTP firmware → `REBOOT mode 1`.
2. Unresponsive firmware or a bare/unknown chip → USB-Serial-JTAG reset-into-bootloader sequence via DTR/RTS (`setSignals` on Web Serial; `SET_CONTROL_LINE_STATE` on the WebUSB shim). Same sequence as esptool's USB-JTAG reset.
3. Fallback instructions with illustration: "Hold BOOT, plug in the dongle, release BOOT, then press Retry".

After entering download mode the USB device re-enumerates. Web Serial needs the user to pick the port again in some browsers; the UI handles that with a single "Select dongle again" step.

**Common safety steps (all flows):**
- **SYNC**, then **chip detect** (read chip ID/magic). Anything other than ESP32-C5 → abort with "This is a <chip>, not an ESP32-C5. Nothing was written."
- Read the flash size (SPI flash ID) and check it against the manifest.
- **Never** touches eFuses. Never enables secure boot or flash encryption.
- Write = `FLASH_BEGIN/DATA/END` per region. Every region is verified afterwards with `SPI_FLASH_MD5` against the manifest. On mismatch: retry the region once, then stop with the dongle left in download mode and the message "Flashing failed, dongle is safe to retry".
- Progress bar with bytes and sectors written. The UI must not be closed during writes (`beforeunload` guard).
- After success: **watchdog reset** (LP_WDT via WRITE_REG; the RTS hard reset does not leave the C5 ROM, M3) → wait for re-enumeration → HELLO → verify `fw_version` and `fw_build_id` equal the manifest. ROM client rules from doc 10 §3 apply: never toggle DTR/RTS, never send CTP bytes to the ROM (probe with SLIP SYNC first), skip FLASH_END.

**Flow A – Update (mismatch dialog or Device tab "Update to bundled"):**
- **Differential:** for each part, compare 4 KB sectors via per-sector `SPI_FLASH_MD5` and write only sectors that differ. Usually only the app changes, and only partly.
- If bootloader or partition-table hashes differ (rare, normally only on MAJOR), these parts are written too. The dialog states it ("bootloader will be updated").

**Flow B – Flash bare / unknown ESP32-C5** (Device tab and start screen: "Flash firmware to a new dongle"):
- Works **without** any CTP firmware present: factory-fresh XIAO, Seeed demo firmware, or a broken or foreign image.
- Steps:
  1. port select;
  2. download mode via (2) or (3);
  3. chip check;
  4. optional **"Erase entire flash first"** checkbox (default off; on for "factory reset");
  5. write all parts (full write, still MD5-verified);
  6. reset and HELLO.
- Mention in UI text: this is the only flow that may overwrite non-CTP firmware. Confirm dialog: "Replace the existing firmware on this ESP32-C5?"

**Flow C – Flash from file (advanced):**
- The user picks a `manifest.json` + `.bin` files, or one merged `.bin` + offset.
- The UI shows version and build_id parsed from the image's app descriptor (`esp_app_desc_t`: version string, IDF version, SHA-256).
- **Warning dialog if MAJOR.MINOR ≠ page**: "This firmware (1.1.0) does not match this page (1.3.x). Flash anyway?" Used by developers and for downgrades.

**Flash wear:** Flow A writes only changed sectors and only when versions differ. Normal operation never writes flash (doc 07 §4). The Device tab shows "sectors written this session".

---

## 12. Log panel and error handling (EVT_LOG, RSP_ERR, local events)

- Unified, time-stamped log:
  - device EVT_LOG (with level);
  - every RSP_ERR (decoded);
  - protocol events (connect, HELLO, retries, reconnects, EVT_RESET, cfg drift);
  - recording events.
- Level filter (error/warn/info/debug), text search, "Copy all", "Save as .txt".
- Ring of last 5 000 lines.
- **Error mapping table** in `protocol.js`: `err_code` → short user message + suggested action. `esp_err` is shown as name + hex (table of common ESP_ERR_* names, e.g. `ESP_ERR_INVALID_ARG 0x102`, `ESP_ERR_WIFI_NOT_STARTED 0x3002`). The `detail` string from the device is shown verbatim (escaped).
- **Error presentation rule:** errors from an action are shown **inline next to the control that caused them** *and* logged. Link-level problems use the top banner. No `alert()`; confirms use `<dialog>`.
- **Command timeouts:** after 3 retries → inline "Dongle didn't respond" + link indicator yellow/red.

---

*Implementation status (2026-11-04):* all sections implemented. §11.1 flashing: `js/romflash.js` (ROM protocol, MD5, differential, watchdog reset) is verified on hardware (release update wrote 2 of 177 sectors in 2.5 s); `js/flasher.js` implements flows A/B/C. Not implemented: "Erase entire flash first" (ERASE_FLASH is a stub-only command; the ROM-only flasher would need a full FLASH_BEGIN erase) and a flash-size check via SPI flash ID.

## 13. Demo mode (simulated dongle)

- "Try without hardware" button on the start screen.
- `demo-device.js` implements CTP v1 **byte-for-byte**: it receives encoded frames and emits COBS+CRC frames. The whole stack (worker, protocol, UI, recording) runs unchanged.
- Generates realistic traffic: beacons from ~20 fake BSSIDs with SSIDs, probe requests, data/ACK, occasional FCS errors. Rates depend on the channel.
- It honours SET_CHANNEL, SET_COUNTRY (rejects channels like real tables), SET_FILTER (same evaluator), SET_SNAPLEN, CAPTURE, stats, EVT_RESET.
- Fault injection toggles (Settings → Developer): drop bytes, corrupt bytes, inject garbage, stall 3 s, simulate reboot, simulate host overrun, saturate (forces drop_ring). These exercise every recovery path in §4.

---

## 14. Settings (persisted in `localStorage`, versioned key `ctk.settings.v1`)

- Desired config (country/mode, band, channel, bw, snaplen, driver masks, active filter set). Restored and pushed on connect.
- Auto-start capture; auto-reconnect.
- Keep the screen on (§18): while connected or recording (default) / always / never.
- Transport override (auto / Web Serial / WebUSB).
- Packet store size.
- Time format; theme; colour rules on/off.
- Recording defaults (name template, split).
- Developer: show raw frames in log, fault injection (demo), PING interval (250–2000 ms), log EVT_LOG level.
- "Reset all settings" with confirm.
- Export/import settings JSON.

---

## 15. First-run & legal notice

Shown once (stored flag), reopenable from the About menu:
- what the tool does;
- receive-only, transmits nothing;
- "Capture only networks you own or are authorised to test. Intercepting others' communications may be illegal (e.g. Finland: communications secrecy provisions)."
- Accept button.

The About menu also shows app version, protocol version and a link to the project docs.

---

## 16. Accessibility & UX rules

- Semantic HTML (`<button>`, `<table>` with roles for the virtualized grid, `<dialog>`, `<details>`, `<label for>`). All controls are keyboard reachable with a visible focus ring.
- The status indicator never relies on colour alone: icon + text label.
- `aria-live="polite"` region for applied/err results; `assertive` for link loss.
- Keyboard shortcuts (shown in a help dialog with `?`):
  - `C` connect;
  - `Space` freeze;
  - `R` record;
  - `S` start/stop capture;
  - `←/→` channel;
  - `/` focus display filter;
  - `Ctrl+E` export buffer.
- Respect `prefers-reduced-motion`.
- All text in English v1. Strings are kept in one `strings.js` object so a Finnish translation can be added without code changes.

## 18. Installable app (PWA)

The page is an installable Progressive Web App (Chrome/Edge on desktop and Android; on Android it then opens full screen from the home screen, which suits USB-OTG field use).

- **Manifest** (`manifest.webmanifest`): `id`/`start_url`/`scope` `./`, `display: standalone`, `orientation: any`, `launch_handler: focus-existing` (one window owns the USB device; a second launch focuses it). Icons: favicon.svg (any), 192/512 PNG (any), 512 PNG maskable (logo inside the 80 % safe zone), Apple touch icon. Screenshots (narrow: packet list and OpenHD FPV; wide: packet list + detail) for the richer install dialog. Shortcut "Demo" → `./#demo`.
- **Regenerating:** `tools/make_icons.sh` (icons from favicon.svg and icons/maskable.svg) and `node tools/pwa_screenshots.mjs` (screenshots from demo mode); both use headless Chromium. Then `make sw-version`.
- **Install button:** when the browser fires `beforeinstallprompt`, "Install as app" appears in the ⋮ menu and on the start screen; hidden when already running standalone. Elsewhere: browser menu → Install / Add to Home screen.
- **`#demo`** in the URL (shortcut, link) starts demo mode; via `launchQueue` too when the app is already open.
- **Offline:** the service worker precaches every web file (docs/11 §3); the installed app starts without network, demo mode included. Firmware images (`fw/*.bin`) are not precached; flashing needs the network once.
- **Updates:** a new version shows the banner "A new version of the app is ready – Reload". Nothing reloads by itself; Reload is refused while recording. The app checks every 30 min and when it comes back to the front.
- **Mobile layout** (< 900 px): one-row top bar (the link text shrinks, the firmware badge shows only as a warning), packet list columns #, time, channel, RSSI, type; no pull-to-refresh (`overscroll-behavior: none`, a reload would end the capture); content kept clear of notches and gesture bars (`viewport-fit=cover` + `env(safe-area-inset-*)`).
- **Keep the screen on** (js/wakelock.js, Screen Wake Lock API): by default while a dongle or the demo is connected (or reconnecting) or a recording runs; settings: always / never. A locked phone suspends the page and the dongle's watchdog stops the capture (§4), and FPV needs the screen anyway. The browser drops the lock when the page is hidden; it is taken again when visible. Refusals (battery saver) only log; the Settings tab says whether the screen is kept on.
- iOS/iPadOS: installable as a home-screen bookmark, but without WebUSB/Web Serial only the demo works (§1).

## 20. Browser capabilities on the start page

When the page loads, the start page shows (and the log records) what this browser can do, from js/capabilities.js `probe()`: browser, version and OS; **which API reaches the dongle and why** (§1); then ✓/✗ for Web Serial (on Android: USB ports works / none offered / untested), WebUSB (dongles already allowed), H.264 and H.265 decoding (WebCodecs `isConfigSupported`, OpenHD video), keep screen on (Wake Lock), offline/install (service worker, https only), and where recordings go (file / browser storage / memory). On Android, while Web Serial is present but unproven and Transport is auto, a *Test Web Serial* button opens the port chooser filtered on the dongle; picking it switches to Web Serial, an empty chooser keeps WebUSB.

## 19. Version information

The start page (below the buttons) and About show, and the log records at startup:
- **Git version** from `version.json`: `Version <tag or git describe> · branch · commit <short> (date)`; "+ uncommitted changes" and "dev server" when served by `make serve`. `version.json` is never committed (a stamp in the commit would describe the previous commit): the Pages deploy writes it (`.github/workflows/pages.yml` → `tools/version_info.py --source pages`, branch from the workflow, tags via a full checkout), and the dev server generates it live from git. Other servers: "Version: unknown".
- **UI files** = the service worker `VERSION` (hash of every precached file, docs/11 §3): identifies exactly the files that are running. From the running service worker (installed app, also offline) or from `sw.js`.
- **Firmware bundle** version and build_id (fw/manifest.json), **installed app / browser tab**, **works offline / online only**.
- The service worker caches `version.json` together with the files, so an installed app shows the version it is actually running, also offline.
- *Copy* puts it on the clipboard with the browser's user agent (for bug reports).
- Like fw/manifest.json, `version.json` is a same-origin request at load time (§0.2).

---

## 17. Protocol feature coverage matrix (acceptance checklist)

| CTP feature (doc 03) | UI element (this doc) |
|---|---|
| COBS framing, resync on 0x00, max frame | worker decoder §2; host counters §9 |
| CRC-32 | worker; CRC error counter §9 |
| Header `ver` | version check, mismatch screen §4 |
| Header `seq` | host loss counter, gap markers §8–§9 |
| Header `req_id` | protocol.js request matching, retries §4/§12 |
| Header `flags.cfg changed`, `cfg_gen` | drift detection §4, list markers §8, pcapng comments §10 |
| HELLO / caps / max_rules / max_frame | connect flow, capability gating §4, editor limits §6, Device tab §11 |
| PING | keepalive, RTT display §4 |
| GET_CONFIG / CONFIG struct | connect flow §4, Device tab §11, rules_crc32 badge §6 |
| SET_CHANNEL (+bw) | Radio panel §5 |
| SET_COUNTRY (validated + manual) | Radio panel §5 |
| SET_FILTER (rules + global conditions) | Filter panel §6, Appendix A |
| SET_SNAPLEN | Capture options §7 |
| SET_DRIVER_MASK | Capture options (advanced) §7 |
| CAPTURE | top bar toggle §7 |
| GET_STATS (+reset) | Stats panel §9 |
| REBOOT normal / download | Device tab §11 |
| HELLO fw_version / fw_flags / fw_build_id, semver rule | mismatch dialog §4.1, top-bar badge, Device tab §11 |
| Firmware update / bare flash / flash from file (ROM protocol) | §11.1 flows A/B/C |
| RSP_OK readback | inline applied values §5–§7 |
| RSP_ERR | inline errors + log + mapping §12 |
| EVT_STATS | Stats panel §9, noise floor §5, snaplen estimate §7 |
| EVT_LOG | Log panel §12 |
| EVT_RESET | banner, auto re-handshake, markers §4/§8/§10 |
| PKT (all fields incl. rx_seq) | list, detail, gap markers, pcapng/radiotap §8/§10 |
| Keepalive watchdog (device stops after 2 s) | background banner §4 |
| Idempotent re-push | Re-push button §11, reconnect §4 |

Every row must be demonstrated in demo mode (§13) and on hardware before v1 release.

---

## Appendix A – SET_FILTER body encoding (adds to doc 03 §6)

Little-endian. Doc 03 left this open; it is defined here and copied into doc 03.
```
u8   n_rules                      (enabled rules only, ≤ max_rules)
u8   gflags                       bit0 drop_fcs_err, bit1 rssi_min_en, bit2 len_min_en, bit3 len_max_en
i8   rssi_min                     dBm
u8   reserved (0)
u16  len_min                      orig_len bytes
u16  len_max
repeat n_rules:
  u8   n_terms                    1..4
  repeat n_terms:
    u16  offset                   from 802.11 FC byte 0
    u8   len                      1..16
    u8   tflags                   bit0 negate
    u8   value[len]
    u8   mask[len]
```
- Evaluation: frame passes if (global conditions pass) AND (n_rules == 0 OR any rule where all terms match).
  - A term matches iff `offset+len ≤ cap-available bytes` AND `∀i (pkt[off+i] & mask[i]) == (value[i] & mask[i])`, XOR negate.
  - Matching uses the full received frame, *before* snaplen truncation.
- `rules_crc32` in CONFIG = CRC-32 (IEEE) over this exact body.
- Max body at the limits = 8 + 16 × (1 + 4 × (4 + 2·16)) = 2 328 bytes. That always fits in one frame (max_frame 4096), so no fragmentation or size budgeting is needed.

## Appendix B – Host-side constants shared with firmware (single table in `protocol.js`)
Message type IDs, err_code list, ESP-IDF promiscuous mask bit values, country code list, channel↔frequency table (2.4: 2407 + 5·ch, ch14 = 2484; 5 GHz: 5000 + 5·ch), DFS channel set per country (for display only).
