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

test('service worker VERSION matches the precached files (make sw-version)', async () => {
  const { swVersion } = await import('../../tools/sw_version.mjs');
  const { want, have } = swVersion();
  assert.equal(have, want, 'sw.js VERSION is stale: run make sw-version');
});

test('PWA manifest: installable icons, screenshots, shortcuts (docs/04 §18)', () => {
  const m = JSON.parse(read('manifest.webmanifest'));
  const png = (p) => { const b = readFileSync(new URL(p, root)); assert.equal(b.toString('latin1', 1, 4), 'PNG', p); return `${b.readUInt32BE(16)}x${b.readUInt32BE(20)}`; };
  for (const k of ['id', 'name', 'short_name', 'start_url', 'scope', 'display', 'background_color', 'theme_color', 'description']) assert.ok(m[k], `manifest ${k}`);
  assert.equal(m.start_url, './'); assert.equal(m.scope, './'); assert.equal(m.display, 'standalone');
  for (const i of m.icons) {
    if (i.type === 'image/png') assert.equal(png(i.src), i.sizes, i.src); else read(i.src);
  }
  const has = (size, purpose) => m.icons.some((i) => i.type === 'image/png' && i.sizes === size && (i.purpose ?? 'any').split(' ').includes(purpose));
  assert.ok(has('192x192', 'any') && has('512x512', 'any') && has('512x512', 'maskable'), 'Chrome needs 192 + 512 PNG and a maskable icon');
  for (const sh of m.screenshots) assert.equal(png(sh.src), sh.sizes, sh.src);
  assert.ok(m.screenshots.some((sh) => sh.form_factor === 'narrow') && m.screenshots.some((sh) => sh.form_factor === 'wide'));
  for (const s of m.shortcuts) { assert.ok(s.url.startsWith('./'), s.url); for (const i of s.icons ?? []) png(i.src); }
  const html = read('index.html');
  assert.ok(html.includes('<link rel="manifest" href="manifest.webmanifest">'));
  png(html.match(/rel="apple-touch-icon" href="([^"]+)"/)[1]);
});
