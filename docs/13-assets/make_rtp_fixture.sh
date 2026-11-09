#!/bin/sh
# Regenerates test/js/fixtures/ohd-rtp.json: GStreamer's x264/x265 encoders and the
# rtph264pay/rtph265pay payloaders OpenHD uses (mtu 1440). Needs gst-plugins-good/-ugly/-bad.
set -eu
out=$(mktemp -d)
cd "$out"
mkdir b264 c265
gst-launch-1.0 -q videotestsrc num-buffers=12 pattern=ball ! video/x-raw,width=320,height=240,framerate=30/1 \
  ! x264enc tune=zerolatency key-int-max=6 bitrate=2000 speed-preset=ultrafast ! video/x-h264,profile=baseline \
  ! h264parse config-interval=-1 ! video/x-h264,stream-format=byte-stream,alignment=au ! tee name=t \
  t. ! queue ! filesink location=es.h264 t. ! queue ! rtph264pay mtu=1440 ! multifilesink location=b264/p%05d.bin
gst-launch-1.0 -q videotestsrc num-buffers=8 pattern=ball ! video/x-raw,format=I420,width=320,height=240,framerate=30/1 \
  ! x265enc tune=zerolatency key-int-max=4 bitrate=1500 speed-preset=ultrafast \
  ! h265parse config-interval=-1 ! video/x-h265,stream-format=byte-stream,alignment=au ! tee name=t \
  t. ! queue ! filesink location=es.h265 t. ! queue ! rtph265pay mtu=1440 ! multifilesink location=c265/p%05d.bin
python3 -I - <<'PY'
import json, glob
d = {}
for c, dirn in (("h264", "b264"), ("h265", "c265")):
    pk = []
    for f in sorted(glob.glob(f"{dirn}/p*.bin")):
        b = open(f, "rb").read()
        while len(b) > 1440:  # multifilesink writes a buffer list as one file
            pk.append(b[:1440]); b = b[1440:]
        pk.append(b)
    seqs = [int.from_bytes(p[2:4], "big") for p in pk]
    assert all((b - a) & 0xFFFF == 1 for a, b in zip(seqs, seqs[1:])), c
    d[c] = {"es": open(f"es.{c}", "rb").read().hex(), "rtp": [p.hex() for p in pk]}
d["generator"] = "GStreamer videotestsrc ! x264enc/x265enc ! h26xparse config-interval=-1 ! rtph26xpay mtu=1440 (the payloaders OpenHD uses)"
json.dump(d, open("rtp-fixture.json", "w"))
PY
echo "$out/rtp-fixture.json"
