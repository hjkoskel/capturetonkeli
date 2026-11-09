#!/usr/bin/env node
// Sets VERSION in sw.js to a hash of every precached file (docs/11 §3), so any change to
// the web UI or a firmware release (fw/manifest.json) installs a new service worker.
// Usage: node tools/sw_version.mjs [--check]   (make sw-version; make js-test checks it)
import { readFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';

const root = new URL('../', import.meta.url);
export function swVersion() {
  const sw = readFileSync(new URL('sw.js', root), 'utf8');
  const files = JSON.parse(sw.match(/const FILES = (\[[\s\S]*?\]);/)[1].replace(/'/g, '"'));
  const h = createHash('sha256');
  for (const f of files) {
    const p = f === './' ? 'index.html' : f;
    h.update(`${p}\0`).update(readFileSync(new URL(p, root))).update('\0');
  }
  return { sw, want: h.digest('hex').slice(0, 16), have: sw.match(/const VERSION = '([^']*)';/)?.[1] };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const { sw, want, have } = swVersion();
  if (process.argv.includes('--check')) {
    if (want !== have) { console.error(`sw.js VERSION is ${have}, files hash to ${want}: run make sw-version`); process.exit(1); }
  } else if (want !== have) {
    writeFileSync(new URL('sw.js', root), sw.replace(/const VERSION = '[^']*';/, `const VERSION = '${want}';`));
    console.log(`sw.js VERSION ${have} → ${want}`);
  } else console.log(`sw.js VERSION ${want} (up to date)`);
}
