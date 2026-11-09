#!/usr/bin/env node
// Regenerates the manifest screenshots (icons/screenshot-*.png, shown in the install
// dialog on Android and desktop) from demo mode. Starts tools/serve.py and a headless
// Chromium of its own on free ports and stops both at the end. Node ≥ 22 (WebSocket).
// Usage: node tools/pwa_screenshots.mjs
import { spawn } from 'node:child_process';
import { writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createServer } from 'node:net';

const root = new URL('../', import.meta.url).pathname;
const wait = (ms) => new Promise((r) => setTimeout(r, ms));
const freePort = () => new Promise((r) => { const s = createServer().listen(0, () => { const p = s.address().port; s.close(() => r(p)); }); });
async function until(fn, ms = 20000) { const t0 = Date.now(); for (;;) { try { const v = await fn(); if (v) return v; } catch { /* not yet */ } if (Date.now() - t0 > ms) throw new Error('timeout'); await wait(200); } }

const [webPort, cdpPort] = [await freePort(), await freePort()];
const profile = mkdtempSync(join(tmpdir(), 'ctk-shots-'));
const chrome = ['chromium', 'chromium-browser', 'google-chrome'].find((c) => { try { return spawn(c, ['--version']).pid; } catch { return false; } });
const server = spawn('python3', ['tools/serve.py', '--port', String(webPort)], { cwd: root, stdio: 'ignore' });
const browser = spawn(chrome, ['--headless=new', `--remote-debugging-port=${cdpPort}`, `--user-data-dir=${profile}`, '--no-first-run', '--enable-unsafe-swiftshader', '--hide-scrollbars', 'about:blank'], { stdio: 'ignore' });
try {
  await until(() => fetch(`http://127.0.0.1:${webPort}/index.html`).then((r) => r.ok));
  const list = await until(() => fetch(`http://127.0.0.1:${cdpPort}/json/list`).then((r) => r.json()));
  const ws = new WebSocket(list.find((t) => t.type === 'page').webSocketDebuggerUrl);
  await new Promise((r) => ws.addEventListener('open', r));
  let id = 0;
  const pend = new Map();
  ws.addEventListener('message', (e) => { const m = JSON.parse(e.data); pend.get(m.id)?.(m); pend.delete(m.id); });
  const cmd = (method, params = {}) => new Promise((r) => { const i = ++id; pend.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
  const ev = (expression) => cmd('Runtime.evaluate', { expression, awaitPromise: true, returnByValue: true });
  const click = (sel) => ev(`document.querySelector(${JSON.stringify(sel)})?.click()`);
  const clickText = (t) => ev(`[...document.querySelectorAll('button')].find((b) => b.offsetParent && b.textContent.startsWith(${JSON.stringify(t)}))?.click()`);

  async function session({ width, height, scale, mobile, fpv }) {
    await cmd('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: scale, mobile });
    await cmd('Page.navigate', { url: `http://127.0.0.1:${webPort}/index.html` });
    await wait(1200);
    await ev(`localStorage.setItem('ctk.settings.v1', JSON.stringify({ acceptedLegal: true, theme: 'dark', fpvSystem: 'openhd', ohdChannel: 149,
      desired: { country: { cc: 'FI', mode: 0, schan: 1, nchan: 13 }, channel: ${fpv ? 149 : 6}, bw: 0, snaplen: 0 } }))`);
    await cmd('Page.reload');
    await wait(1500);
    await click('#start-demo');
    await wait(1500);
    await clickText('▶ Capture');
    if (fpv) {
      await click('[data-tab=fpv]');
      await wait(300);
      await clickText('Tune dongle');
      await wait(500);
      await clickText('Use OpenHD capture filter');
      await wait(500);
      await ev(`(() => { const c = document.querySelector("canvas[aria-label='OpenHD video']"); let el = c.parentElement;
        while (el && el.scrollHeight <= el.clientHeight) el = el.parentElement;
        if (el) el.scrollTop += c.getBoundingClientRect().top - el.getBoundingClientRect().top - 8; })()`);
    }
    await wait(6000);
  }
  async function shot(file) {
    const r = await cmd('Page.captureScreenshot', { format: 'png' });
    writeFileSync(join(root, file), Buffer.from(r.result.data, 'base64'));
    console.log(`wrote ${file}`);
  }
  await session({ width: 360, height: 780, scale: 1.5, mobile: true });
  await shot('icons/screenshot-narrow.png');
  await session({ width: 360, height: 780, scale: 1.5, mobile: true, fpv: true });
  await shot('icons/screenshot-fpv-narrow.png');
  await session({ width: 1280, height: 800, scale: 1, mobile: false });
  await ev(`[...document.querySelectorAll('.prow[data-n]')].at(-4)?.click()`);
  await wait(500);
  await shot('icons/screenshot-wide.png');
  ws.close();
} finally {
  browser.kill();
  server.kill();
  await wait(500);
  rmSync(profile, { recursive: true, force: true });
}
