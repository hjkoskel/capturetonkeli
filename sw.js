// Service Worker (docs/11 §3): offline + "always latest" (installable PWA, docs/04 §18).
// Navigation and fw/manifest.json: network-first (3 s), fallback cache.
// Everything else: cache-first within one cache per version. VERSION is a hash of
// every file in FILES (tools/sw_version.mjs, checked by make js-test), so any UI change
// changes sw.js, the browser installs the new worker and the page offers "Reload".
const VERSION = '5db1f3735b53970f';
const FILES = ['./', 'index.html', 'css/app.css', 'favicon.svg', 'manifest.webmanifest', 'icons/icon-192.png', 'icons/icon-512.png', 'icons/maskable-512.png', 'icons/apple-touch-icon.png', 'js/main.js', 'js/app.js', 'js/codec.js', 'js/wire.js',
  'js/filter.js', 'js/semver.js', 'js/dot11.js', 'js/pcapng.js', 'js/protocol.js', 'js/errors.js', 'js/transport.js', 'js/demo-device.js',
  'js/worker.js', 'js/display-filter.js', 'js/regdom.js', 'js/settings.js', 'js/log.js', 'js/ui/dom.js', 'js/ui/radio.js', 'js/ui/filterpanel.js',
  'js/ui/stats.js', 'js/ui/device.js', 'js/ui/logpanel.js', 'js/ui/settingspanel.js', 'js/ui/packets.js', 'js/romflash.js', 'js/flasher.js', 'js/hxfec.js', 'js/hxfpv.js', 'js/hxdemo.js', 'js/ui/fpv.js', 'js/ohdcrypto.js', 'js/ohdfec.js', 'js/openhd.js', 'js/ohdkeys.js', 'js/ohdkeys-worker.js', 'js/mavlink.js', 'js/mavlink-crc.js', 'js/rtp.js', 'js/ohddemo.js', 'js/ui/openhd.js', 'js/ui/flip.js', 'js/mp4.js', 'js/wakelock.js', 'js/diversity.js', 'js/ui/group.js', 'js/version.js', 'js/capabilities.js', 'fw/manifest.json'];

const CACHE = `ctk-${VERSION}`; // fw/manifest.json is in FILES, so a release changes VERSION too

self.addEventListener('install', (e) => {
  e.waitUntil((async () => {
    const c = await caches.open(CACHE);
    // bypass the HTTP cache so a new version never precaches stale files
    await c.addAll(FILES.map((f) => new Request(f, { cache: 'reload' })));
    // git version of these files (written by the Pages deploy; absent elsewhere): cached with them,
    // so the start page shows the version that is running, also offline (docs/04 §19)
    await c.add(new Request('version.json', { cache: 'reload' })).catch(() => {});
  })());
});

self.addEventListener('activate', (e) => {
  e.waitUntil((async () => {
        for (const k of await caches.keys()) if (k.startsWith('ctk-') && k !== CACHE) await caches.delete(k);
    await self.clients.claim();
  })());
});

// The page asks the waiting worker to take over when the user taps "Reload" (never automatically).
self.addEventListener('message', (e) => {
  if (e.data?.t === 'skip-waiting') self.skipWaiting();
  if (e.data?.t === 'version') e.source?.postMessage({ t: 'version', version: VERSION });
});

const timeout = (ms) => new Promise((_, rej) => setTimeout(() => rej(new Error('timeout')), ms));

self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin) return;
  const nav = e.request.mode === 'navigate', networkFirst = nav || url.pathname.endsWith('/fw/manifest.json');
  e.respondWith((async () => {
    const cache = await caches.open(CACHE);
    if (networkFirst) {
      try { return await Promise.race([fetch(e.request), timeout(3000)]); } catch {
        // offline: any navigation inside the scope (e.g. ./?x or a shortcut) gets the app shell
        return (await cache.match(e.request, { ignoreSearch: nav })) ?? (nav ? await cache.match('./') : null) ?? Response.error();
      }
    }
    return (await cache.match(e.request)) ?? fetch(e.request);
  })());
});
