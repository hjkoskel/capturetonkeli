// Service Worker (docs/11 §3): offline + "always latest".
// Navigation and fw/manifest.json: network-first (3 s), fallback cache.
// Everything else: cache-first within a cache named after the release build_id.
const FILES = ['./', 'index.html', 'css/app.css', 'favicon.svg', 'manifest.webmanifest', 'js/main.js', 'js/app.js', 'js/codec.js', 'js/wire.js',
  'js/filter.js', 'js/semver.js', 'js/dot11.js', 'js/pcapng.js', 'js/protocol.js', 'js/errors.js', 'js/transport.js', 'js/demo-device.js',
  'js/worker.js', 'js/display-filter.js', 'js/regdom.js', 'js/settings.js', 'js/log.js', 'js/ui/dom.js', 'js/ui/radio.js', 'js/ui/filterpanel.js',
  'js/ui/stats.js', 'js/ui/device.js', 'js/ui/logpanel.js', 'js/ui/settingspanel.js', 'js/ui/packets.js', 'js/romflash.js', 'js/flasher.js', 'js/hxfec.js', 'js/hxfpv.js', 'js/hxdemo.js', 'js/ui/fpv.js', 'js/ohdcrypto.js', 'js/ohdfec.js', 'js/openhd.js', 'js/ohdkeys.js', 'js/ohdkeys-worker.js', 'js/mavlink.js', 'js/mavlink-crc.js', 'js/rtp.js', 'js/ohddemo.js', 'js/ui/openhd.js', 'js/ui/flip.js', 'fw/manifest.json'];

async function buildId() {
  try { const r = await fetch('fw/manifest.json', { cache: 'no-cache' }); if (r.ok) return (await r.json()).build_id || 'dev'; } catch { /* offline */ }
  return 'dev';
}

self.addEventListener('install', (e) => {
  e.waitUntil((async () => {
    const c = await caches.open(`ctk-${await buildId()}`);
    await c.addAll(FILES);
  })());
});

self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
    const keep = `ctk-${await buildId()}`;
    for (const k of await caches.keys()) if (k.startsWith('ctk-') && k !== keep) await caches.delete(k);
    await self.clients.claim();
  })());
});

const timeout = (ms) => new Promise((_, rej) => setTimeout(() => rej(new Error('timeout')), ms));

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin) return;
  const networkFirst = e.request.mode === 'navigate' || url.pathname.endsWith('/fw/manifest.json');
  e.respondWith((async () => {
    if (networkFirst) {
      try { return await Promise.race([fetch(e.request), timeout(3000)]); } catch { return (await caches.match(e.request)) ?? Response.error(); }
    }
    return (await caches.match(e.request)) ?? fetch(e.request);
  })());
});
