# fpvview – OpenHD / hx-esp32-cam-fpv viewer and recorder

An example program for the Go library (docs/05 §11). It shows the video of an **OpenHD**
drone (docs/13) or an **hx-esp32-cam-fpv** air unit (docs/12) in a window and records it.
The video is received passively with one or more capturetonkeli dongles; nothing is
transmitted.

```
dongle(s) ─ ctp.Group ─▶ 802.11 frames ─▶ openhd.Receiver ─RTP─▶ video.Depacketizer ─▶ ffmpeg ─▶ raylib window
                                      └─▶ hxfpv.Receiver ─JPEG─▶ image/jpeg ───────────────────▶ raylib window
                                         access units / JPEGs ─▶ video.Recorder (.mp4, .h264/.h265) / .mjpeg
```

This is a **separate Go module**, so the library itself stays stdlib-only. It uses
[raylib-go](https://github.com/gen2brain/raylib-go) (cgo) for the window.

## Build

Needs a C compiler and the OpenGL/X11 development headers (Debian/Ubuntu: `libgl1-mesa-dev
libxi-dev libxcursor-dev libxrandr-dev libxinerama-dev`; Arch: `mesa libxi libxcursor libxrandr
libxinerama`). OpenHD video is decoded with **ffmpeg** (`ffmpeg` on the PATH, or `-ffmpeg`).

```
make fpvview          # from the repository root, or:
cd examples/fpvview && go build
```

## Use

```
fpvview -system openhd -country FI -channel 140              # OpenHD on 5700 MHz
fpvview -system openhd -freq 5700 -record flight.mp4         # same, recording an MP4
fpvview -system openhd -bind-phrase mysecret                 # custom bind phrase (Argon2id, ~0.3 s)
fpvview -system hx -channel 7 -record cam.mjpeg              # hx-esp32-cam-fpv, Motion JPEG
fpvview -system hx -port /dev/ttyACM0,/dev/ttyACM1           # two dongles: receive diversity
fpvview -system openhd -flip rot180 -fullscreen              # camera mounted upside down
fpvview -system openhd -demo                                 # simulated drone, no dongle
fpvview -pcap capture.pcapng -system openhd                  # replay a web UI / ctpbench capture
```

`fpvview -h` lists every flag. Keys in the window:

| Key | |
|---|---|
| Alt+Enter, F11 | toggle window / full screen |
| O | info overlay on/off |
| S | save a screenshot (`fpvview-YYYYMMDD-HHMMSS.png`) |
| Esc, Q | quit (recordings are finished on exit) |

## Notes

- **Dongles:** `-port auto` (default) uses every dongle found (up to 4). With more than one
  the frames are merged (`ctp.Group`, docs/14), and the OpenHD FEC waits 15 ms for late
  copies. The dongle gets the system's capture filter (`openhd.CaptureFilter`,
  `hxfpv.CaptureFilter`), snaplen 0 and the channel; `-country` sets the regulatory country
  (5 GHz channels need one that allows them).
- **OpenHD video:** low latency settings for ffmpeg (minimal probing, one thread for
  decoding and output, an access unit delimiter after every frame). Real cameras send key
  frames rarely or never (intra refresh), so the decoder starts with the cached parameter
  sets when no key frame came within 2 s, and after dropping frames (ffmpeg behind) it
  resumes within 1 s even without a key frame. `-v` logs what the decoder does;
  `-demo-keyint` sets the demo's key frame interval (default 300 frames).
- **Recording** happens before decoding, so it does not depend on the window.
  - OpenHD `.mp4` is written moov-first; the frames are spooled to `NAME.mp4.part` and the
    file is assembled when the camera changes its SPS (continuing in `NAME-2.mp4`) or on
    exit. `.h264`/`.h265` is the raw stream.
  - hx `.mjpeg` is the JPEG frames back to back (`ffplay -f mjpeg cam.mjpeg`). A directory
    gets numbered `.jpg` files.
- **Demo:** `-demo` simulates the air unit in the process, with the library's senders and
  `-demo-loss` on the "air". The OpenHD demo encodes ffmpeg's test source live
  (`-demo-codec h264|h265`, needs libx264/libx265).
