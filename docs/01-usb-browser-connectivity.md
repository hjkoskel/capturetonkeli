# 01 – USB ↔ browser connectivity for XIAO ESP32-C5 (research, 2026-11-04)

## Hardware reality
- ESP32-C5 has **no USB-OTG controller**, only the **USB Serial/JTAG (USJ) controller**: a fixed-function, hardware-only USB device. It can only be a CDC-ACM serial port + JTAG. No custom descriptors, no HID, no vendor/WebUSB BOS descriptor, no NCM/RNDIS network gadget, no mass storage.
- VID:PID fixed 0x303A:0x1001. USB 2.0 **Full-Speed (12 Mbit/s)** only. D+ = GPIO14, D− = GPIO13.
- XIAO ESP32-C5's USB-C is wired straight to the USJ (no USB-UART bridge chip) → class-compliant CDC-ACM, no driver on Win10+/macOS/Linux/ChromeOS.
- USJ quirks: if the TX buffer fills with no host reading, firmware waits ~50 ms once; host→device data is flow-controlled; the device disappears from the bus in deep sleep; pin reconfiguration kills USB.

## Browser paths (no driver install)
| Host | API | Status |
|---|---|---|
| Win/macOS/Linux/ChromeOS – Chrome/Edge/Opera | **Web Serial** | Stable since Chromium 89 |
| Desktop Firefox | **Web Serial** | Shipped in Firefox 151 (May 2026) |
| Android – Chrome | **WebUSB** + own CDC-ACM layer in JS | Works: Chrome uses the Android USB host API (and can detach a kernel cdc_acm driver where a phone ships one, see doc 08 A2). Proven by projects talking to ESP32-C6 USJ this way |
| Android – Chrome | Web Serial | Shipped in Chrome 148, but in practice lists only Bluetooth RFCOMM ports, not USB CDC devices → do not rely on it |
| Desktop – WebUSB to CDC | ✗ | OS cdc-acm/usbser/AppleUSBCDC driver owns the interface; claimInterface fails |
| iOS/iPadOS Safari | ✗ | WebKit rejects Web Serial/WebUSB |
| Android WebView | ✗ | No WebUSB/Web Serial |

→ **Architecture: one JS transport interface with two backends**: `navigator.serial` on desktop, `navigator.usb` + small CDC-ACM driver (SET_LINE_CODING, SET_CONTROL_LINE_STATE, bulk IN/OUT) on Android. Same byte stream, same framing protocol above it.

## Bandwidth / latency budget
- FS bulk theoretical ≈ 1.1 MB/s; ESP32 CDC measurements in the wild 0.5–0.9 MB/s. Must benchmark C5 USJ ourselves.
- A busy 802.11 channel exceeds this easily → device-side filtering + snaplen truncation + compact binary framing are mandatory, not optional.
- Latency: FS bulk scheduled per 1 ms frame; USJ sends in 64-byte packets. Keep several IN transfers queued on WebUSB side (≈16 in flight) to avoid starving.

## Rejected alternatives
- Wi-Fi SoftAP + WebSocket: same radio as capture; forces channel lock, breaks hopping, adds own traffic to capture.
- BLE + Web Bluetooth: far too slow, shares radio time with Wi-Fi, no Firefox/Safari.
- Swapping to ESP32-S3/P4 for OTG (WebUSB vendor class / NCM): possible but S3 has no 5 GHz; C5 is the only small dual-band part → stick with CDC.

## Open risks to test on hardware
1. DTR/RTS toggling on port open can trigger USJ auto-reset/download-mode → set signals explicitly after open, test on each OS.
2. Real sustained USJ throughput on C5 and CPU cost of feeding it while promiscuous callback runs.
3. Android OTG power budget and whether Chrome's WebUSB picker shows 303A:1001 on target phones.

## Sources
- Espressif ESP-IDF, USB Serial/JTAG Controller Console (ESP32-C5): https://docs.espressif.com/projects/esp-idf/en/v5.5/esp32c5/api-guides/usb-serial-jtag-console.html
- Espressif, Establish Serial Connection with ESP32-C5: https://docs.espressif.com/projects/esp-idf/en/latest/esp32c5/get-started/establish-serial-connection.html
- Zephyr XIAO ESP32C5 board doc: https://docs.zephyrproject.org/latest/boards/seeed/xiao_esp32c5/doc/index.html
- WHATWG serial repo (implementation status): https://github.com/whatwg/serial
- Chrome 148 release notes: https://developer.chrome.com/release-notes/148
- Mozilla blog, Web Serial in Firefox 151: https://blog.mozilla.org/en/firefox/firefox-web-serial-adafruit/
- lightplayer PR #1191 (ESP32-C6 USJ over WebUSB on Android): https://github.com/PhotomancerArt/lightplayer/pull/1191
- RadioFoundry PR #312 (Android Web Serial lists only Bluetooth): https://github.com/RadioFoundry/RadioFoundry/pull/312
- Chromium blink-dev, WebUSB interface class filtering / OS drivers: https://groups.google.com/a/chromium.org/g/blink-dev/c/LZXocaeCwDw
- esp32.com USB speed test thread: https://esp32.com/viewtopic.php?p=137425
