// Device tab → "Dongles (receive diversity)" (docs/14 §5): add/remove dongles, labels,
// per-dongle contribution, clock lock, drops, and the shared-USB-link hint (§3).
import { app, MAX_DONGLES } from '../app.js';
import { h, inline, toast, fmtNum, fmtBytes } from './dom.js';

const pct = (a, b) => (b > 0 ? `${((100 * a) / b).toFixed(1)} %` : '–');

export function initGroup(root) {
  const table = h('table.stats-table'), summary = h('div.small'), hint = h('div'), msg = h('div');
  const addBtn = h('button', { onclick: () => add('pick') }, 'Add dongle…');
  const addDemoBtn = h('button', { onclick: () => add('demo') }, 'Add demo dongle');
  root.append(
    h('h3', 'Dongles (receive diversity)'),
    h('p.small.muted', 'Up to 4 dongles on the same channel work as one: a frame missed by one antenna comes from another. Every dongle gets the same channel, filter and snaplen. The packet list shows each air frame once (the first copy, without delay); the packet detail shows the RSSI at each dongle.'),
    h('div.row', addBtn, addDemoBtn),
    table, summary, hint, msg,
  );

  async function add(mode) {
    addBtn.disabled = addDemoBtn.disabled = true;
    try {
      const d = await app.addMember(mode);
      inline(msg, d);
      if (!d) toast(`${app.members.length} dongles in the group`);
    } finally { draw(); }
  }

  // rates, once per worker tick (2/s)
  let prev = null, prevT = 0, rates = new Map(), group = null;
  const prevDrop = new Map();
  app.addEventListener('host', (e) => {
    const g = e.detail.group, now = performance.now(), dt = prevT ? (now - prevT) / 1000 : 0;
    rates = new Map();
    const out = g?.active && prev?.active && dt ? g.out - prev.out : null;
    for (const m of app.members) {
      const w = g?.members.find((x) => x.id === m.id), mg = w?.merge, pm = prev?.members?.find((x) => x.id === m.id)?.merge;
      const drop = m.devStats?.dropRing, pd = prevDrop.get(m.id);
      if (drop !== undefined) prevDrop.set(m.id, drop);
      rates.set(m.id, {
        pps: w?.pps ?? null, bps: w?.bps ?? 0, merge: mg,
        unique: mg && pm && out ? pct(mg.unique - pm.unique, out) : '–',
        first: mg && pm && out ? pct(mg.first - pm.first, out) : '–',
        drop: drop !== undefined && pd !== undefined && dt ? (drop - pd) / dt : null,
      });
    }
    group = out !== null ? { outPs: out / dt, dupPer: (g.dups - prev.dups) / Math.max(1, out), rescuedPs: (g.rescued - prev.rescued) / dt } : null;
    prev = g; prevT = now;
    draw();
  });
  app.addEventListener('members', draw);
  app.addEventListener('conn', draw);

  function draw() {
    const conn = app.conn === 'connected', mems = app.members, n = mems.length, demo = app.transport?.kind === 'demo';
    addBtn.classList.toggle('hidden', demo);
    addDemoBtn.classList.toggle('hidden', !demo);
    addBtn.disabled = addDemoBtn.disabled = !conn || n >= MAX_DONGLES;
    if (!conn || !n) { table.replaceChildren(); summary.textContent = 'Connect a dongle first, then add more.'; hint.replaceChildren(); return; }

    const rows = mems.map((m) => {
      const r = rates.get(m.id) ?? {}, mg = r.merge;
      const state = m === app.primary ? (n > 1 ? 'primary' : 'connected') : m.state === 'ok' ? 'merged' : m.state === 'degraded' ? 'not merged: settings differ' : m.state;
      const clock = !mg ? '–' : mg.ref ? 'reference' : mg.clock.locked
        ? `locked ±${Math.max(1, Math.round(mg.clock.rmsUs))} µs, ${mg.clock.ppm >= 0 ? '+' : ''}${mg.clock.ppm.toFixed(1)} ppm`
        : `aligning (${mg.clock.anchors} anchors)`;
      const name = h('input', { type: 'text', value: m.label, maxLength: 24, style: { width: '8em' }, 'aria-label': `Name of dongle ${m.mac}`, title: 'Name (e.g. "patch N", "omni"); kept per MAC in this browser',
        onchange: (e) => app.setLabel(m, e.target.value.trim()) });
      return h('tr',
        h('td', name),
        h('td.mono.small', m.mac || '…'),
        h(m.state === 'degraded' ? 'td.warn-text' : 'td', state, m.error && m.state === 'degraded' ? h('div.small', m.error.title) : null),
        h('td', r.pps === null || r.pps === undefined ? '–' : fmtNum(Math.round(r.pps))),
        h('td', r.unique ?? '–'), h('td', r.first ?? '–'), h('td.small', clock),
        h(r.drop > 0 ? 'td.warn-text' : 'td', r.drop === null || r.drop === undefined ? '–' : fmtNum(Math.round(r.drop))),
        h('td', n > 1 ? h('button.small', { onclick: async () => { inline(msg, await app.removeMember(m)); draw(); } }, 'Remove') : null));
    });
    table.replaceChildren(
      h('thead', h('tr', ...['dongle', 'MAC', 'state', 'pkt/s', 'only this one', 'first', 'clock', 'dropped/s', ''].map((t) => h('th', t)))),
      h('tbody', ...rows));

    const bytes = [...rates.values()].reduce((a, x) => a + (x.bps || 0), 0);
    summary.textContent = n < 2 ? 'One dongle. Add another one on a separate USB port (other antenna or position) for diversity.'
      : !group ? 'Group: starting…'
        : `Group: ${fmtNum(Math.round(group.outPs))} frames/s out · ${group.dupPer.toFixed(2)} extra copies per frame · ${fmtNum(Math.round(group.rescuedPs))}/s bad copies replaced by a good one · USB ${fmtBytes(bytes)}/s in total`;
    // docs/14 §3: full-speed dongles behind one single-TT hub share 12 Mbit/s
    const dropping = [...rates.values()].some((x) => x.drop > 0);
    hint.replaceChildren(...(n > 1 && dropping && bytes < 1.3e6
      ? [h('div.inline-msg.warn', 'The dongles drop frames while their USB total stays near one dongle\'s maximum: they may share one USB link (a single-TT hub). Use separate USB ports of the computer or a multi-TT hub; or filter more / lower the snaplen.')] : []));
  }
  draw();
}
