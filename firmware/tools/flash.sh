#!/usr/bin/env bash
# Flash firmware/build to the dongle without relying on the DTR/RTS
# reset-into-bootloader sequence, which proved unreliable on hardware (M3):
#   1. CTP firmware answers  → REBOOT 1 (firmware forces ROM download mode)
#   2. ROM already answers   → (BOOT held while plugging in)
#   3. last resort           → esptool's USB-JTAG DTR/RTS sequence
# Leaves ROM with a watchdog reset (the RTS "hard reset" did not reset the chip).
# Usage: flash.sh CTPBENCH [PORT|auto]
set -euo pipefail
B=$1; PORT=${2:-auto}
cd "$(dirname "$0")/../${FW_BUILD:-build}"
if ! command -v esptool >/dev/null 2>&1; then . "$IDF_PATH/export.sh" >/dev/null; fi

find_port() {
  if [ "$PORT" != auto ]; then echo "$PORT"; return; fi
  "$B" list --json | python3 -c 'import json,sys; d=[p["path"] for p in json.load(sys.stdin)["ports"] if p["is_dongle"]]; print(d[0] if len(d)==1 else "")'
}
rom_ok() { timeout 15 esptool --chip esp32c5 -p "$1" --before no-reset --after no-reset chip-id >/dev/null 2>&1; }

p=$(find_port); [ -n "$p" ] || { echo "flash: need exactly one dongle (or PORT=...)" >&2; exit 3; }
before=no-reset
if "$B" --port "$p" info --timeout 3s >/dev/null 2>&1; then
  echo "flash: CTP firmware on $p → REBOOT 1 (download mode)"
  "$B" --port "$p" reboot --download --yes >/dev/null
  sleep 2; [ "$PORT" = auto ] && p=$(find_port)
elif rom_ok "$p"; then
  echo "flash: ROM download mode already active on $p"
else
  echo "flash: no CTP answer and no ROM; trying esptool's DTR/RTS sequence" >&2
  before=default-reset
fi
if ! esptool --chip esp32c5 -p "$p" --before "$before" --after watchdog-reset write-flash @flash_args; then
  echo "flash: failed. Hold BOOT while plugging the dongle in, then run make flash again." >&2
  exit 10
fi
sleep 2
exec "$B" --port "$([ "$PORT" = auto ] && echo auto || echo "$PORT")" wait-device --timeout 15s
