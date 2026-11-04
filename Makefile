# capturetonkeli – top-level entry points (docs/07 §7, docs/11 §4).
# Targets are placeholders until milestone M0 implements them; each fails loudly instead of pretending to pass.

.PHONY: help host-test fuzz-short diff-test fw-host-conf fw flash hil ci lint release serve

help:
	@echo "Targets (see docs/07 §7, docs/11 §4):"
	@echo "  host-test     C core unit tests + sanitizers (C11 and gnu23)"
	@echo "  fuzz-short    libFuzzer, 60 s per target"
	@echo "  diff-test     C vs Go reference differential tests"
	@echo "  fw-host-conf  firmware core on a PTY + ctpbench conformance"
	@echo "  fw            build firmware (native IDF if matching firmware/idf-version.txt, else Podman)"
	@echo "  flash         flash dongle from host, then ctpbench wait-device"
	@echo "  hil           ctpbench conformance + benches vs baselines (hardware)"
	@echo "  ci            what CI runs"
	@echo "  release       VERSION=X.Y.Z: build bundle into fw/"
	@echo "  serve         serve web UI on http://localhost:8000"

serve:
	python3 -m http.server 8000

host-test fuzz-short diff-test fw-host-conf fw flash hil ci lint release:
	@echo "make $@: not implemented yet (milestone M0, docs/07 §5)" >&2; exit 2
