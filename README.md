# capturetonkeli

A cheap USB dongle that gives **any laptop or Android phone Wi-Fi monitor mode**: no driver patches, no special adapters.

A Seeed **XIAO ESP32-C5** (dual-band 2.4 / 5 GHz Wi-Fi 6) captures 802.11 frames in promiscuous mode. It filters them on the device using byte-pattern rules, optionally truncates them, and streams them with low latency over USB to:
- a **static web page** (vanilla HTML/JS, served from GitHub Pages) using **Web Serial** on desktop Chrome/Edge/Firefox and **WebUSB** on Android Chrome;
- **`ctpbench`**, a stdlib-only Go command-line tool and library.

Captures are saved as pcapng with radiotap headers, ready for Wireshark. The dongle only receives; it never transmits.

## Status
Milestones M0–M2 (no hardware needed) implemented: Go reference library + simulator + conformance suite, the portable firmware core running on a PC pseudo-terminal, and the ESP-IDF firmware build. Hardware bring-up (M3+) and the web UI are next; see [docs/07](docs/07-firmware-plan.md).

```
make ci                                   # everything that runs without hardware
go run ./cmd/ctpbench --port sim info     # talk to the simulator
make fw-host-conf                         # real firmware core on a PTY + conformance
```

## Repository layout
| Path | Content |
|---|---|
| `index.html`, `css/`, `js/`, `sw.js` | Web UI (GitHub Pages site root) |
| `fw/` | Current release firmware bundle served to the web UI and embedded in ctpbench (generated) |
| `go.mod`, `ctp/`, `cmd/ctpbench/`, `conformance/` | Go library, test bench, conformance suite |
| `firmware/` | ESP-IDF v6 firmware (C11) |
| `testvectors/` | Golden vectors shared by Go, JS and C tests |
| `docs/` | Specifications 01–11 |

## Documentation
1. [USB ↔ browser connectivity](docs/01-usb-browser-connectivity.md)
2. [Capture filtering & radio configuration](docs/02-capture-filtering-and-radio-config.md)
3. [USB link protocol (CTP)](docs/03-usb-link-protocol.md)
4. [Web UI specification](docs/04-web-ui-spec.md)
5. [Go library specification](docs/05-go-library-spec.md)
6. [Test bench (ctpbench) specification](docs/06-testbench-spec.md)
7. [Firmware plan](docs/07-firmware-plan.md)
8. [Open research items](docs/08-open-research-items.md)
9. [RAM boot vs auto-update decision](docs/09-ram-boot-vs-auto-update.md)
10. [ESP-IDF v6 API audit](docs/10-idf-v6-api-audit.md)
11. [GitHub repo, Pages & release](docs/11-github-repo-and-release.md)

## Legal
Capture only networks you own or are authorised to test. Intercepting other people's communications may be illegal where you live.

## Licence
To be decided (see docs/08 D2).
