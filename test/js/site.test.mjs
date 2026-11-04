// docs/11 web-check: relative URLs only, no external URLs, SW precache list complete.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';

const root = new URL('../../', import.meta.url);
const read = (p) => readFileSync(new URL(p, root), 'utf8');
const jsFiles = ['js', 'js/ui'].flatMap((d) => readdirSync(new URL(d, root)).filter((f) => f.endsWith('.js')).map((f) => `${d}/${f}`));

test('service worker precaches every web file', () => {
  const sw = read('sw.js');
  const list = JSON.parse(sw.match(/const FILES = (\[[\s\S]*?\]);/)[1].replace(/'/g, '"'));
  for (const f of [...jsFiles, 'index.html', 'css/app.css', 'favicon.svg', 'manifest.webmanifest']) assert.ok(list.includes(f), `${f} missing from sw.js FILES`);
  for (const f of list) if (f !== './') read(f); // every listed file exists
});

test('no absolute or external URLs in the web UI', () => {
  for (const f of ['index.html', 'css/app.css', 'manifest.webmanifest', ...jsFiles]) {
    const s = read(f);
    assert.ok(!/(?:src|href)=["']\/(?!\/)/.test(s), `${f}: absolute path`);
    assert.ok(!/(?:src|href|url\(|import\s*\(?\s*)["']?https?:\/\//.test(s), `${f}: external URL`);
    assert.ok(!/from\s+["']\//.test(s), `${f}: absolute import`);
  }
});
