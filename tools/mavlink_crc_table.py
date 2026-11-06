#!/usr/bin/env python3
"""Regenerates js/mavlink-crc.js from a checkout of https://github.com/OpenHD/mavlink-headers.

Usage: python3 -I tools/mavlink_crc_table.py /path/to/mavlink-headers > js/mavlink-crc.js
Takes MAVLINK_MESSAGE_CRCS of the openhd and ardupilotmega dialects (both include common)
and fails on conflicting entries.
"""
import re, sys, textwrap

root = sys.argv[1]
t = {}
for d in ("openhd", "ardupilotmega"):
    s = open(f"{root}/mavlink/v2.0/{d}/{d}.h").read()
    m = re.search(r"#define MAVLINK_MESSAGE_CRCS \{(.*)\}\n", s).group(1)
    for e in re.findall(r"\{(\d+), (\d+), (\d+), (\d+),", m):
        i, c, mn, mx = map(int, e)
        if i in t and t[i] != (c, mn, mx):
            sys.exit(f"conflict for message {i}: {t[i]} vs {(c, mn, mx)} ({d})")
        t[i] = (c, mn, mx)
body = "\n".join(textwrap.wrap(" ".join(f"{i}:{c}:{mn}:{mx}" for i, (c, mn, mx) in sorted(t.items())), 110))
print("// Generated from OpenHD/mavlink-headers (openhd + ardupilotmega dialects, both include common):")
print("// msgid:crc_extra:min_len:max_len. Regenerate with tools/mavlink_crc_table.py.")
print(f"export const CRC_TABLE = `\n{body}`;")
