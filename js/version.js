// Version information (docs/04 §19), shown on the start page and in About, and logged:
//  - git:  version.json (written by the Pages deploy, served live by the dev server):
//          describe, tag, branch, commit, commit date; absent on other servers
//  - ui:   hash of every precached web file (sw.js VERSION): exactly which files are running
//  - fw:   the firmware bundled with this page (fw/manifest.json)
//  - how:  installed app or browser tab; offline-capable (service worker) or not
const standalone = () => matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;

async function gitInfo() {
  try {
    const r = await fetch('version.json', { cache: 'no-cache' }); // installed app: the copy cached with these files
    if (!r.ok) return null;
    const j = await r.json();
    return j?.commit ? j : null;
  } catch { return null; }
}

/** The running service worker's VERSION, else the one in sw.js on the server. */
async function uiHash() {
  const sw = navigator.serviceWorker?.controller;
  if (sw) {
    const v = await new Promise((resolve) => {
      const t = setTimeout(() => resolve(null), 1000);
      const on = (e) => { if (e.data?.t === 'version') { clearTimeout(t); navigator.serviceWorker.removeEventListener('message', on); resolve(e.data.version); } };
      navigator.serviceWorker.addEventListener('message', on);
      sw.postMessage({ t: 'version' });
    });
    if (v) return v;
  }
  try { return (await (await fetch('sw.js', { cache: 'no-cache' })).text()).match(/const VERSION = '([^']*)'/)?.[1] ?? null; } catch { return null; }
}

export async function versionInfo(manifest) {
  const [git, ui] = await Promise.all([gitInfo(), uiHash()]);
  return { git, ui, fw: manifest ? { version: manifest.version, buildId: manifest.build_id } : null, standalone: standalone(), offline: !!navigator.serviceWorker?.controller };
}

/** [headline, details] for display. */
export function versionLines(v) {
  const g = v.git;
  const day = (iso) => (iso ? iso.slice(0, 10) : '');
  const head = !g ? 'Version: unknown (this server provides no version.json)'
    : `Version ${g.tag || g.describe}${g.branch ? ` · branch ${g.branch}` : ''} · commit ${g.short}${g.date ? ` (${day(g.date)})` : ''}${g.dirty ? ' + uncommitted changes' : ''}${g.source === 'dev-server' ? ' · dev server' : ''}`;
  const parts = [
    v.ui ? `UI files ${v.ui}` : 'UI files: unknown',
    v.fw ? `firmware bundle ${v.fw.version} (build ${v.fw.buildId})` : 'no firmware bundle (development page)',
    v.standalone ? 'installed app' : 'browser tab',
    v.offline ? 'works offline' : 'online only',
  ];
  return [head, parts.join(' · ')];
}
