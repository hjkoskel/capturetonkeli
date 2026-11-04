# 05 – Go host library specification "ctp-go" (v1 draft, 2026-11-04)

Native host-side implementation of the capturetonkeli link protocol (CTP v1, doc 03) for Go programs.
Uses: alternative clients (CLI, daemons, Wireshark extcap, GUI), the firmware test bench (doc 06), and the **reference implementation** that the firmware and the web UI (doc 04) are tested against.

---

## 0. Hard constraints

1. **Go standard library only.**
   - No third-party modules; `go.mod` has no `require` lines. Not even `golang.org/x/sys`.
   - Serial ports are opened with raw syscalls (`syscall` package) per OS.
2. Go ≥ 1.22. `log/slog` for logging, `context` everywhere a call can block, `errors.Is/As`-compatible errors.
3. Pure Go, `CGO_ENABLED=0` builds for linux/{amd64,arm64,arm}, darwin/{amd64,arm64}, windows/amd64. Runs on a Raspberry Pi.
4. The library never prints. Diagnostics go only through an optional `*slog.Logger`.
5. Every exported type that crosses the wire has round-trip tests against the **shared test vectors** (§9). Those vectors are the single source of truth for Go, JS and firmware.

## 1. Module & package layout

Module root = **repository root** (`go.mod` at repo root, module `github.com/<owner>/capturetonkeli`; full repo layout in doc 11 §1). Paths below are relative to the repo root.
```
fw/                         package `fw`: //go:embed of the current release bundle (manifest.json + *.bin), doc 11 §1
ctp/                        high-level Client (session, requests, keepalive, events, packets)
  ctp/codec/                COBS, CRC-32, frame encode/decode, streaming Decoder
  ctp/wire/                 message type IDs, header, all message structs, error codes, constants
  ctp/filter/               filter rule model, JSON format, SET_FILTER encoder, reference matcher
  ctp/serialport/           stdlib-only serial: Open, List, DTR/RTS control (linux, darwin, windows)
  ctp/pcapng/               pcapng + radiotap writer (and minimal reader, for tests)
  ctp/dot11/                minimal 802.11 header / IE decoder (type names, addrs, SSID)
  ctp/sim/                  simulated dongle: device side of CTP over any io.ReadWriter
  ctp/fwver/                semver parse/compare, compatibility rule, manifest.json model, app-descriptor parser
  ctp/romflash/             ESP ROM download protocol: reset-to-bootloader, SYNC, chip detect, FLASH_*, MD5, differential flash
testvectors/                *.json golden vectors shared with firmware + web (see §9)
conformance/                conformance suite cases (importable; used by ctpbench and go test vs sim)
cmd/ctpbench/               test bench CLI (doc 06); default expected firmware = fw package (embedded)
```
(Package directories above are `ctp/codec/`, `ctp/wire/`, … under the repo root; the indentation shows nesting.)
Lower packages never import higher ones: `codec ← wire ← filter ← ctp`, `sim` uses `codec/wire/filter`.

---

## 2. `codec` – framing

```go
func COBSEncode(dst, src []byte) []byte          // appends; dst may be nil
func COBSDecode(dst, src []byte) ([]byte, error) // src = one frame without delimiter
func MaxEncodedLen(n int) int                    // n + n/254 + 2

const MaxFrame = 4096 + 32

// EncodeFrame appends COBS(header|body|crc32) + 0x00 to dst.
func EncodeFrame(dst []byte, h wire.Header, body []byte) []byte

// Decoder is a streaming, allocation-free decoder.
type Decoder struct { /* ring/scratch buffers */ }
func NewDecoder(maxFrame int) *Decoder
// Feed consumes bytes. For each complete valid frame it calls fn with the header and the body.
// The body slice is valid only during the callback.
func (d *Decoder) Feed(p []byte, fn func(h wire.Header, body []byte)) 
func (d *Decoder) Stats() DecoderStats // Frames, CRCErrors, COBSErrors, Oversize, SkippedBytes, Resyncs
```
- CRC-32 = `hash/crc32` IEEE table.
- Resync rule exactly as doc 03 §3: discard until the first `0x00`; empty frames are ignored.
- **Fuzz test (`go test -fuzz`) is mandatory:** no panics, bounded memory, recovery after the next delimiter.

## 3. `wire` – messages

- Constants for every message type, flag bit, cap bit, err_code, reset_reason, bb_format, bandwidth, reboot mode and gflags/tflags bit. The names are the same as in doc 03; Appendix B of doc 04 mirrors this list.
- One struct per message body, each with `AppendBinary(dst []byte) []byte` and `UnmarshalBinary(b []byte) error`.
  - Unmarshal accepts **longer** bodies (ignores the trailing bytes; doc 03 §11 forward-compat rule).
  - Unmarshal accepts **shorter** bodies when the missing fields are trailing optional ones (default value).
  - It rejects truncated mandatory fields with `ErrShortBody`.
```go
type Header struct { Ver, Type uint8; Seq, ReqID uint16; Flags, CfgGen uint8 }
type Hello struct { HostNonce uint32; HostProtoVer uint8 }
type DeviceInfo struct { ProtoVer uint8; FwVersion uint32 /* packed semver, see fwver */; FwFlags uint8; FwBuildID [8]byte;
                         MAC [6]byte; Caps Caps; MaxFrame, MaxRules uint16; DeviceTimeUS uint64; HostNonce uint32;
                         ResetReason ResetReason; StateFlags uint8 }
type Config struct { CfgGen, Channel uint8; BW Bandwidth; Band Band; Country Country; Snaplen uint16;
                     TypeMask, CtrlMask uint32; CaptureOn bool; NRules uint8; RulesCRC32 uint32 }
type Country struct { CC [3]byte; Mode CountryMode; SChan, NChan uint8 }
type RspErr struct { Code ErrCode; EspErr int32; Detail string }
type Stats struct { DeviceTimeUS uint64; RxTotal, RxPassed, DropRing, DropFCS, TxFrames, TxBytes,
                    HostRxCRCErr, HostRxOversize uint32; RingHighWater uint16; Channel uint8; NoiseFloor int8; FlashWrites uint32 }
type Log struct { Level uint8; Text string }
type Reset struct { Reason ResetReason; FwVersion uint32; StateFlags uint8; Detail string }
type PktMeta struct { TsUS uint64; Channel, Flags uint8; RSSI, Noise int8; Rate, BBFormat uint8;
                      OrigLen, CapLen, RxSeq uint16 }
```
- `ErrCode.String()` and `EspErrName(int32) string` (table of common ESP_ERR_* values).
- `ChannelFreqMHz(ch) (mhz int, band Band, ok bool)` and `FreqChannel(mhz)`.

## 4. `filter` – rules (identical semantics to firmware and web)

```go
type Term struct { Offset uint16; Value, Mask []byte; Negate bool }
type Rule struct { Name string; Enabled bool; Terms []Term }
type Global struct { DropFCSErr bool; RSSIMin *int8; LenMin, LenMax *uint16 }
type Set struct { Version int; Rules []Rule; Global Global }

const MaxRules, MaxTerms, MaxTermLen = 16, 4, 16

func (s Set) Validate(maxRules int) error           // all limits, value/mask length equality
func (s Set) Encode() ([]byte, error)               // SET_FILTER body, enabled rules only
func (s Set) CRC32() (uint32, error)                // CRC of Encode(); compare to Config.RulesCRC32
func Decode(body []byte) (Set, error)               // inverse (for sim + tests)
func (s Set) Match(frame []byte, m wire.PktMeta) bool  // REFERENCE evaluator (doc 04 App. A)
func ParseTerm(offset uint16, pattern string) (Term, error) // "80", "8?", "aa:bb:cc:dd:ee:ff", "88-8e ?? 03"
```
- JSON (`encoding/json`) uses **exactly the web UI import/export format** `{version:1, rules:[…], global:{…}}`. Hex strings are written in the `ParseTerm` syntax, so filter files move freely between browser, CLI and tests.
- Presets as constructors: `Beacons()`, `ProbeReq()`, `ProbeResp()`, `Mgmt()`, `Ctrl()`, `Data()`, `DropBeacons()`, `FromMAC(mac)`, `ToMAC(mac)`, `BSSID(mac)`, `DeauthDisassoc()`, `EAPOL()` (returns 2 rules). Same definitions as web presets.
- `Match` is the oracle. The firmware's C matcher and the JS matcher must agree with it on every `testvectors/filter_*.json` case.

## 5. `serialport` – stdlib-only serial

```go
type Port interface { io.ReadWriteCloser; SetDTR(bool) error; SetRTS(bool) error; SetReadTimeout(time.Duration) error }
type Info struct { Path string; VID, PID uint16; Serial, Product string }
func Open(path string) (Port, error)  // raw mode 8N1, no flow control, no echo; baud is irrelevant for USJ
func List() ([]Info, error)           // all USB serial ports with VID/PID when known
func FindDongles() ([]Info, error)    // List filtered to VID 0x303A PID 0x1001
```
| OS | Open | List |
|---|---|---|
| Linux | `syscall.Open` O_RDWR\|O_NOCTTY, `ioctl` TCGETS/TCSETS2 raw mode, VMIN/VTIME, TIOCMBIS/TIOCMBIC for DTR/RTS, `TIOCEXCL` exclusive lock | walk `/sys/class/tty/*/device/../` (`idVendor`, `idProduct`, `serial`, `product`) |
| macOS | `syscall.Open` `/dev/cu.*`, `ioctl` TIOCGETA/TIOCSETA, TIOCMBIS/TIOCMBIC, TIOCEXCL | glob `/dev/cu.usbmodem*`. VID/PID unknown, so identify by HELLO probe (`FindDongles` opens and probes) |
| Windows | `syscall.CreateFile("\\\\.\\COMn")`, `SetCommState`/`SetCommTimeouts`/`EscapeCommFunction` loaded via `syscall.NewLazyDLL("kernel32.dll")` | `syscall.RegOpenKeyEx` `HKLM\HARDWARE\DEVICEMAP\SERIALCOMM` + `HKLM\SYSTEM\CurrentControlSet\Enum\USB\VID_303A&PID_1001\*` for the COM mapping |

- **DTR/RTS on open:** set to the safe state identified in doc 01 risk #1 (to be confirmed on hardware), exposed as `serialport.SafeOpenSignals`. `ctp.Dial` applies it automatically.
- Exclusive open is required. A second opener fails with `ErrPortBusy`, which protects against two agents or tools sharing one dongle.

## 6. `ctp` – Client

```go
type Options struct {
    Logger          *slog.Logger      // nil = silent
    RequestTimeout  time.Duration     // default 200 ms (doc 03 §5)
    Retries         int               // default 3
    PingInterval    time.Duration     // default 500 ms; 0 disables (only for tests)
    PacketQueue     int               // default 65536 packets buffered towards the user
    OnFull          FullPolicy        // DropNewest (default) | Block (tests only)
    Desired         *DesiredConfig    // if set: pushed after HELLO and re-pushed on reset/drift
    AutoReconnect   bool              // reopen same path after link loss (Dial only)
    HostProtoVer    uint8             // default 1
    HostFwVersion   fwver.Version     // firmware version this host is built/tested with (from the bundled manifest)
    VersionPolicy   VersionPolicy     // Strict (default): MAJOR.MINOR mismatch → New/Dial return *FirmwareMismatchError
                                      // AllowMismatch: connect anyway, Client.Unsupported() == true, EventVersionMismatch emitted
}
type DesiredConfig struct { Country *wire.Country; Channel *uint8; BW *wire.Bandwidth; Snaplen *uint16;
                            TypeMask, CtrlMask *uint32; Filter *filter.Set; Capture *bool }

func Dial(ctx context.Context, path string, o Options) (*Client, error) // "auto" = first FindDongles() hit
func New(ctx context.Context, rw io.ReadWriteCloser, o Options) (*Client, error) // any transport (pipe, TCP, sim)

func (c *Client) Info() wire.DeviceInfo
func (c *Client) GetConfig(ctx) (wire.Config, error)
func (c *Client) SetCountry(ctx, wire.Country) (wire.Config, error)
func (c *Client) SetChannel(ctx, ch uint8, bw wire.Bandwidth) (wire.Config, error)
func (c *Client) SetFilter(ctx, filter.Set) (wire.Config, error)     // verifies RulesCRC32 == local CRC
func (c *Client) SetSnaplen(ctx, n uint16) (wire.Config, error)
func (c *Client) SetDriverMask(ctx, typ, ctrl uint32) (wire.Config, error)
func (c *Client) Capture(ctx, on bool) (wire.Config, error)
func (c *Client) Stats(ctx, reset bool) (wire.Stats, error)
func (c *Client) Ping(ctx) (time.Duration, error)
func (c *Client) Reboot(ctx, mode wire.RebootMode) error            // expects link drop; returns after RSP_OK
func (c *Client) Do(ctx, typ uint8, body []byte) (wire.Header, []byte, error) // raw escape hatch (test bench)

func (c *Client) Packets() <-chan *Packet
func (c *Client) Events() <-chan Event
func (c *Client) Counters() HostCounters
func (c *Client) LinkState() LinkState
func (c *Client) Close() error                                       // CAPTURE 0 best effort, then close
```

### Behaviour (normative, mirrors doc 03 §5–§7 and doc 04 §4)
- **Goroutines:**
  - one reader: transport → `codec.Decoder` → dispatch;
  - one writer: serialized requests;
  - one keepalive ticker.
  - No goroutine per request.
- **req_id** allocation is monotonic and wraps, skipping 0. Pending requests sit in a map. A retry resends the **same req_id** (device dedups).
- **Timeouts:** per-request deadline = min(ctx deadline, RequestTimeout × (Retries+1)). After that the call returns `ErrTimeout`. If the keepalive fails 3× in a row → `LinkDead` event, and all pending calls fail with `ErrLinkDead`.
- **Setters return the readback `Config`.** If the readback differs from the request (e.g. BW downgraded) the call still succeeds, and the caller compares. `SetFilter` returns `ErrFilterMismatch` if the CRC differs.
- **Version checks** (doc 03 §11):
  - `DeviceInfo.ProtoVer != HostProtoVer` → `New` fails with `*ProtocolVersionError`. No override: the only way forward is `romflash`.
  - `fwver.Compatible(HostFwVersion, DeviceInfo.FwVersion) == false` and `VersionPolicy == Strict` → `New` fails with `*FirmwareMismatchError{Device, Host fwver.Version; DeviceBuildID [8]byte}`. The connection is closed cleanly; the caller decides (prompt, flash, or retry with `AllowMismatch`).
  - PATCH-only difference → connects; `EventVersionInfo` notes it.
- **cfg_gen tracking:** an unexpected change → `EventConfigDrift` + GET_CONFIG. If `Desired` is set → automatic re-push (`EventRepushed`).
- **EVT_RESET** → `EventDeviceReset` → HELLO again + re-push.
- **Gap detection:**
  - header `seq` gaps → `HostCounters.LinkLost` + `EventGap{Where: Host}`;
  - PKT `rx_seq` gaps → `EventGap{Where: Device, N}`.

### Packet
```go
type Packet struct {
    wire.PktMeta
    Data    []byte      // captured 802.11 bytes (len = CapLen)
    CfgGen  uint8
    HostRx  time.Time   // host receive time (monotonic)
}
func (p *Packet) Release()  // optional: return buffer to pool; Data invalid afterwards
func (p *Packet) Dot11() dot11.Header  // lazy decode helper
```
- Steady state: **zero allocations per packet** when the consumer calls `Release()`. Buffers come from a `sync.Pool` sized by `MaxFrame`.
- If the consumer is slow and `PacketQueue` is full: DropNewest + `HostCounters.QueueDrops++`. The reader never blocks, so keepalive keeps working.

### Events
```go
type Event interface{ isEvent() }
EventStats{wire.Stats} · EventLog{wire.Log} · EventDeviceReset{wire.Reset} · EventLink{State LinkState} ·
EventGap{Where GapWhere; N uint32} · EventConfigDrift{Old, New wire.Config} · EventRepushed{Config wire.Config}
```

### Errors
```go
var ErrTimeout, ErrLinkDead, ErrClosed, ErrPortBusy, ErrShortBody, ErrFilterMismatch error
type DeviceError struct { wire.RspErr; Request uint8 }   // errors.As target for RSP_ERR
type ProtocolVersionError struct { Device, Host uint8 }
type FirmwareMismatchError struct { Device, Host fwver.Version; DeviceBuildID [8]byte; DeviceFlags uint8 }
```

## 7. `pcapng` & `dot11`

- `pcapng.NewWriter(w io.Writer, o WriterOptions)`:
  - SHB (with `shb_userappl`, `shb_hardware`) and IDB LINKTYPE 127, `if_tsresol` = 6 (µs).
  - `WritePacket(p *ctp.Packet, comment string)` builds the radiotap header (TSFT, Flags incl. FCS-at-end/bad-FCS, Rate or MCS, Channel, dBm signal, dBm noise) and the EPB with the original length.
  - The byte layout is identical to the web UI writer; tests compare both against `testvectors/pcapng_*.json`.
- `pcapng.Reader` is a minimal reader used by tests and the bench (`ctpbench verify-pcap`).
- `dot11.Parse(b []byte) Header`: FC fields, type/subtype name, addr1–4, seq/frag, QoS, flags, plus `IEs()` iterator over mgmt bodies (SSID, DS channel, RSN present, vendor OUI). Never panics on truncated input (fuzzed).

## 7b. `fwver` and `romflash`

```go
// fwver
type Version struct { Major, Minor uint8; Patch uint16 }
func Parse(s string) (Version, error)                 // "1.3.0", "v1.3.0"
func FromWire(u uint32) Version; func (v Version) Wire() uint32
func Compatible(host, dev Version) bool               // Major==Major && Minor==Minor (doc 03 §11)
type Manifest struct { Version string; BuildID string; ProtocolVer int; IDF, Chip string; Flash FlashParams; Parts []Part }
func LoadManifest(fsys fs.FS, path string) (Manifest, error)  // works with os.DirFS and embed.FS
func ParseAppDesc(app []byte) (AppDesc, error)        // esp_app_desc_t: version, idf_ver, sha256 → used for "flash from file"

// romflash
type Flasher struct { /* port, logger */ }
func EnterBootloader(ctx, port serialport.Port, how ResetMethod) error  // CTP REBOOT 1 | USB-JTAG DTR/RTS sequence | manual
func Connect(ctx, port serialport.Port) (*Flasher, error)              // SYNC + chip detect
func (f *Flasher) Chip() ChipInfo                                      // must be ESP32-C5 for our images
func (f *Flasher) FlashSize(ctx) (int, error)
func (f *Flasher) WriteParts(ctx, m fwver.Manifest, fsys fs.FS, o WriteOptions) (Report, error)
//   WriteOptions{ Differential bool; EraseAll bool; Progress func(done, total int) }
//   Report{ SectorsWritten, SectorsSkipped int; PerPart []PartResult }  (MD5-verified per part)
func (f *Flasher) HardReset() error
```
- **Same protocol behaviour as the JS flasher** (doc 04 §11.1), including the chip check, the eFuse ban, MD5 verification and retry-once. Shared test vectors: `testvectors/romproto.json` (SLIP frames of each command and response).
- `sim` gains a minimal ROM-loader emulation (SYNC, chip ID, FLASH_*, MD5 over an in-memory flash), so flashing flows are tested without hardware. It also lets an "update" switch the simulated firmware version.

## 8. `sim` – simulated dongle (device side)

The same component as the web demo device (doc 04 §13), implemented natively in Go:
```go
type Device struct { /* state: config, filter, counters, ring */ }
type SimOptions struct {
    Caps wire.Caps; MaxRules uint16; FwVersion uint32; MAC [6]byte
    Countries map[string][]uint8        // allowed channels per country (rejects like ESP-IDF)
    Traffic  TrafficModel               // BSS list, rates per channel, FCS error rate; seeded RNG
    USBBytesPerSec int                  // models the USB bottleneck → drop_ring behaviour
    Faults   Faults                     // DropBytes, CorruptBytes, Garbage, Stall, RebootAt, ...
    Seed     int64                      // deterministic runs
}
func NewDevice(o SimOptions) *Device
func (d *Device) Serve(ctx context.Context, rw io.ReadWriter) error   // speaks CTP device side
func Pipe(o SimOptions) (host io.ReadWriteCloser, dev *Device)        // in-memory pair for tests
```
- Implements every message, the dedup cache (last 4 req_ids), ctrl-before-packet priority, the 2 s host watchdog, `cfg_gen`, EVT_STATS each second, and EVT_RESET on simulated reboot.
- **Deterministic:** a fixed seed plus a virtual clock option (`SimOptions.Clock`) makes tests reproducible and fast (no real sleeps).
- Uses `filter.Match`, so sim behaviour is by definition the reference behaviour.

## 9. Shared test vectors (`testvectors/`)

JSON files consumed by Go tests, JS tests (`node --test` or the browser test page) and firmware unit tests (Unity, host-compiled C):
| File | Content |
|---|---|
| `cobs.json` | `{in_hex, out_hex}` incl. edge cases (empty, all-zero, 254/255/256-byte runs) |
| `crc32.json` | inputs and expected CRCs |
| `frames.json` | header + body → full wire bytes; plus malformed streams with expected decoder stats |
| `messages.json` | each message type: struct JSON ↔ body hex (incl. forward-compat longer bodies) |
| `filter_encode.json` | filter JSON → SET_FILTER body hex + CRC |
| `filter_match.json` | filter + frame hex + meta → expected bool (≥ 200 cases incl. negate, out-of-range offsets, FCS, RSSI, len) |
| `pcapng.json` | packet → expected EPB bytes |

`go generate ./...` (re)creates the vectors from the Go reference. Any change shows up as a reviewable diff.

## 10. Quality gates

- `go vet`, `go test -race ./...`, `go test -fuzz` targets for `codec.Decoder`, `wire.Unmarshal*`, `filter.Decode`, `dot11.Parse` (CI runs each 60 s).
- Benchmarks (`go test -bench`), on a laptop class CPU:
  - Decoder ≥ 200 MB/s;
  - full Client path ≥ 100k PKT/s with 0 allocs/op;
  - `filter.Match` ≤ 100 ns for 16×4 rules.
- An end-to-end Client↔sim test covers every doc 04 §17 row that applies to a host (same coverage matrix).
- Public API documented with Go doc comments and runnable `Example*` functions (they double as usage docs for agents).
