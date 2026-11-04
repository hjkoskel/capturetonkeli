# capturetonkeli – top-level entry points (docs/07 §7, docs/11 §4).
# All targets are non-interactive; the exit code is the result.
-include local.mk

SHELL      := /bin/bash
HOST_DIR   := firmware/host
HB         := $(HOST_DIR)/build
IDF_PIN    := $(shell cat firmware/idf-version.txt)
IDF_IMAGE  := docker.io/espressif/idf:$(IDF_PIN)
CONTAINER  ?= podman
BUILD_ENV  ?= auto
IDF_VERSION_CHECK ?= 1
FUZZ_SECS  ?= 60
CTPBENCH   := bench-out/bin/ctpbench
PORT       ?= auto
CC_HOST    ?= cc

CORE_SRC   := firmware/components/ctp_core

.PHONY: help host-test fuzz-short diff-test fw-host fw-host-conf fw fw-smoke flash flash-container \
        hil ci lint go-test vectors ctpbench release serve doctor clean

help:
	@echo "Targets (docs/07 §7, docs/11 §4):"
	@echo "  host-test     C core unit tests + ASan/UBSan, C11 and gnu23 (L0+L1)"
	@echo "  fuzz-short    fuzz targets, FUZZ_SECS=$(FUZZ_SECS) s each (libFuzzer if clang works)"
	@echo "  diff-test     C filter vs Go reference, 100k random cases (L3)"
	@echo "  fw-host-conf  ctp_fw_host on a PTY + ctpbench conformance (L4, M2 gate)"
	@echo "  go-test       go vet + go test -race ./..."
	@echo "  lint          layering greps, gofmt, cppcheck, clang-tidy, gcc -fanalyzer"
	@echo "  fw            idf.py build (BUILD_ENV=auto|native|podman)"
	@echo "  fw-smoke      IDF API smoke build + Kconfig check (docs/10 §5)"
	@echo "  flash         flash from the host, then ctpbench wait-device (PORT=auto)"
	@echo "  hil           ctpbench conformance on the dongle (L6)"
	@echo "  ci            lint host-test fuzz-short diff-test go-test fw-host-conf fw fw-smoke"
	@echo "  serve         serve web UI on http://localhost:8000"

# ---- Go ------------------------------------------------------------------------

ctpbench:
	@mkdir -p $(dir $(CTPBENCH))
	go build -o $(CTPBENCH) ./cmd/ctpbench

vectors:
	go generate ./...

go-test:
	go vet ./...
	go test -race -count=1 ./...

# ---- C host build (never needs IDF) ------------------------------------------------

$(HB)-c11/ctp_tests $(HB)-gnu23/ctp_tests: FORCE
	@std=$$(basename $(@D) | sed 's/.*build-//'); \
	cmake -S $(HOST_DIR) -B $(@D) -DCTP_C_STD=$$std -DCMAKE_C_COMPILER=$(CC_HOST) >/dev/null && cmake --build $(@D)

host-test: $(HB)-c11/ctp_tests $(HB)-gnu23/ctp_tests
	$(HB)-c11/ctp_tests
	$(HB)-gnu23/ctp_tests

FORCE:

# libFuzzer needs a working clang; otherwise the standalone gcc driver runs.
CLANG_OK := $(shell clang --version >/dev/null 2>&1 && echo yes)
fuzz-short:
ifeq ($(CLANG_OK),yes)
	cmake -S $(HOST_DIR) -B $(HB)-fuzz -DCMAKE_C_COMPILER=clang >/dev/null && cmake --build $(HB)-fuzz
	@for t in frame filter messages; do \
	  mkdir -p bench-out/fuzz/$$t; \
	  $(HB)-fuzz/fuzz_$$t -max_total_time=$(FUZZ_SECS) -print_final_stats=1 bench-out/fuzz/$$t || exit 1; done
else
	@echo "fuzz-short: clang not usable, using the gcc standalone driver" >&2
	cmake -S $(HOST_DIR) -B $(HB)-c11 -DCTP_C_STD=c11 >/dev/null && cmake --build $(HB)-c11
	@for t in frame filter messages; do $(HB)-c11/fuzz_$$t $(FUZZ_SECS) || exit 1; done
endif

diff-test: $(HB)-c11/ctp_tests
	CTP_FILTER_DIFF=$(CURDIR)/$(HB)-c11/filter_diff go test -count=1 -v -run Differential ./internal/difftest/

# ---- firmware in the loop (L4) ---------------------------------------------------------

fw-host: $(HB)-c11/ctp_tests

fw-host-conf: fw-host ctpbench
	@mkdir -p bench-out; rm -f bench-out/fw-host.pty; \
	$(HB)-c11/ctp_fw_host --link bench-out/fw-host.pty > bench-out/fw-host.out 2> bench-out/fw-host.err & pid=$$!; \
	for i in $$(seq 50); do [ -e bench-out/fw-host.pty ] && break; sleep 0.1; done; \
	$(CTPBENCH) --port bench-out/fw-host.pty conformance --skip '$(CONF_SKIP)' --out-dir bench-out/fw-host-conf; rc=$$?; \
	kill $$pid 2>/dev/null; wait $$pid 2>/dev/null; \
	if grep -q "ERROR: \|runtime error" bench-out/fw-host.err; then echo "sanitizer report in bench-out/fw-host.err" >&2; rc=1; fi; \
	exit $$rc

# M2 gate skips RF-injector and RTT-bound cases (docs/07 M2); override with CONF_SKIP=
CONF_SKIP ?= CTP-(FLT-RF|LIVE-RTT)

# ---- static analysis (L0) ---------------------------------------------------------------

CPPCHECK_FLAGS := --enable=all --inline-suppr --std=c11 --check-level=exhaustive --error-exitcode=1 -q \
  -I $(CORE_SRC)/include -I $(CORE_SRC)/src --suppress=missingIncludeSystem --suppress=checkersReport \
  --suppress=unusedFunction --suppress=staticFunction

lint:
	@echo "lint: layering"; \
	! grep -rnE '#include *[<"](esp_|freertos/|driver/)' $(CORE_SRC) || { echo "IDF include in ctp_core" >&2; exit 1; }; \
	! grep -rnE '\b(malloc|calloc|realloc|free)\s*\(' $(CORE_SRC) || { echo "heap use in ctp_core" >&2; exit 1; }; \
	! grep -rnE '(^|[^_A-Z])assert\s*\(' $(CORE_SRC) firmware/components/ctp_port_esp firmware/main || { echo "assert() used; use CTP_ASSERT" >&2; exit 1; }; \
	! grep -rnE 'GPIO_NUM_1[34]|usb_phy|USB_PHY' firmware/components firmware/main || { echo "never touch GPIO13/14 or the USB PHY" >&2; exit 1; }
	@test -z "$$(gofmt -l ctp cmd conformance internal testvectors)" || { gofmt -l ctp cmd conformance internal testvectors; echo "gofmt needed" >&2; exit 1; }
	cppcheck $(CPPCHECK_FLAGS) $(CORE_SRC)/src
ifeq ($(shell clang-tidy --version >/dev/null 2>&1 && echo yes),yes)
	clang-tidy --quiet $(CORE_SRC)/src/*.c -- -std=c11 -I$(CORE_SRC)/include -I$(CORE_SRC)/src  # config: $(CORE_SRC)/.clang-tidy
else
	@echo "lint: clang-tidy not usable, skipped" >&2
endif
	cmake -S $(HOST_DIR) -B $(HB)-analyzer -DCTP_ANALYZER=ON -DCTP_SANITIZE=OFF >/dev/null && cmake --build $(HB)-analyzer --target ctp_core

# ---- ESP-IDF build: native IDF if it matches the pin, else the pinned image (docs/07 §7.1) ----

IDF_LOCAL := $(shell if command -v idf.py >/dev/null 2>&1; then idf.py --version 2>/dev/null; \
  elif [ -n "$$IDF_PATH" ] && [ -f "$$IDF_PATH/export.sh" ]; then bash -c '. "$$IDF_PATH/export.sh" >/dev/null 2>&1 && idf.py --version' 2>/dev/null; fi | grep -oE 'v[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1)

ifeq ($(BUILD_ENV),auto)
  ifeq ($(IDF_LOCAL),)
    ENV_SEL := podman
  else ifeq ($(IDF_LOCAL),$(IDF_PIN))
    ENV_SEL := native
  else ifeq ($(IDF_VERSION_CHECK),0)
    ENV_SEL := native
  else
    ENV_SEL := mismatch
  endif
else
  ENV_SEL := $(BUILD_ENV)
endif

IDF_NATIVE = if command -v idf.py >/dev/null 2>&1; then idf.py $(1); else bash -c '. "$$IDF_PATH/export.sh" >/dev/null && idf.py $(1)'; fi
IDF_PODMAN = $(CONTAINER) run --rm --userns=keep-id -e IDF_GIT_SAFE_DIR=/project -e HOME=/tmp \
  -v ctk-idf-cache:/cache:U -e CCACHE_DIR=/cache/ccache -e IDF_CCACHE_ENABLE=1 \
  -v "$(CURDIR)":/project:Z -w /project/$(2) $(IDF_IMAGE) idf.py $(1)

define idf_build
	@case "$(ENV_SEL)" in \
	  native) echo "build-env: native (IDF $(IDF_LOCAL) at $$IDF_PATH)"; cd $(2) && $(call IDF_NATIVE,$(1)) ;; \
	  podman) echo "build-env: podman ($(IDF_IMAGE))"; $(call IDF_PODMAN,$(1),$(2)) ;; \
	  mismatch) echo "build-env: error: local IDF $(IDF_LOCAL) != pinned $(IDF_PIN): run 'make $@ BUILD_ENV=podman', or check out the pinned IDF" >&2; exit 2 ;; \
	  *) echo "BUILD_ENV must be auto|native|podman" >&2; exit 2 ;; \
	esac
endef

fw:
	$(call idf_build,build,firmware)

fw-smoke:
	$(call idf_build,build,firmware/test_apps/idf_api_smoke)
	python3 firmware/test_apps/idf_api_smoke/gen_smoke.py kconfig firmware/idf-api.txt firmware/test_apps/idf_api_smoke/build/config/sdkconfig.json

# Flashing always runs on the host (docs/07 §7.1). Prefers REBOOT 1 over the
# DTR/RTS sequence (unreliable on hardware, see docs/10 §3 M3 findings).
flash: ctpbench
	firmware/tools/flash.sh $(CURDIR)/$(CTPBENCH) $(PORT)

flash-container:
	$(CONTAINER) run --rm --userns=keep-id --group-add keep-groups --device $(PORT) \
	  -v "$(CURDIR)":/project:Z -w /project/firmware $(IDF_IMAGE) idf.py -p $(PORT) flash

hil: ctpbench
	$(CTPBENCH) --port $(PORT) conformance --skip 'CTP-FLT-RF' --out-dir bench-out/last

doctor: ctpbench
	@cmake --version | head -1; python3 --version; go version; echo "IDF local: $(IDF_LOCAL:%=%) pinned: $(IDF_PIN) -> $(ENV_SEL)"
	-$(CTPBENCH) --port $(PORT) doctor

ci: lint host-test fuzz-short diff-test go-test fw-host-conf fw fw-smoke

release:
	@echo "make release: not implemented yet (milestone M7, docs/07 §5)" >&2; exit 2

serve:
	python3 -m http.server 8000

clean:
	rm -rf $(HB) $(HB)-* firmware/build firmware/test_apps/*/build bench-out
