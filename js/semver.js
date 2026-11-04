// Firmware versions and the compatibility rule (docs/03 §11).

/** "1.3.0", "v1.3.0", "0.1.0-dev" → {major, minor, patch, pre}; null if not semver. */
export function parse(s) {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:-(.+))?$/.exec(String(s).trim());
  if (!m) return null;
  return { major: +m[1], minor: +m[2], patch: +m[3], pre: m[4] ?? '' };
}

export const toString = (v) => `${v.major}.${v.minor}.${v.patch}${v.pre ? '-' + v.pre : ''}`;

/** Compatible iff MAJOR and MINOR match; PATCH and pre-release never matter. */
export const compatible = (host, dev) => host.major === dev.major && host.minor === dev.minor;

/** <0, 0, >0 on MAJOR.MINOR.PATCH. */
export const compare = (a, b) => a.major - b.major || a.minor - b.minor || a.patch - b.patch;
