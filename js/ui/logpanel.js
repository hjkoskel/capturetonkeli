// Log panel (docs/04 §12): level filter, search, copy, save.
import { h, download, toast } from './dom.js';
import { all, subscribe, fmt, text, LEVELS } from '../log.js';

export function initLog(root) {
  const lvl = h('select', { 'aria-label': 'Level' }, LEVELS.map((l) => h('option', { value: l }, `${l} and above`)));
  lvl.value = 'info';
  const q = h('input', { type: 'search', placeholder: 'Search', 'aria-label': 'Search log' });
  const box = h('div.logbox', { role: 'log', 'aria-live': 'off' });
  root.append(
    h('div.row', lvl, q),
    h('div.row', h('button.small', { onclick: copy }, 'Copy all'), h('button.small', { onclick: () => download('capturetonkeli-log.txt', text(), 'text/plain') }, 'Save as .txt')),
    box);

  const show = (e) => LEVELS.indexOf(e.level) <= LEVELS.indexOf(lvl.value) && (!q.value || fmt(e).toLowerCase().includes(q.value.toLowerCase()));
  const line = (e) => h(`div.${e.level}`, fmt(e));
  function redraw() { box.replaceChildren(...all().filter(show).slice(-2000).map(line)); box.scrollTop = box.scrollHeight; }
  lvl.onchange = q.oninput = redraw;
  subscribe((e) => {
    if (!show(e)) return;
    const stick = box.scrollTop + box.clientHeight >= box.scrollHeight - 20;
    box.append(line(e));
    while (box.childElementCount > 2000) box.firstChild.remove();
    if (stick) box.scrollTop = box.scrollHeight;
  });
  async function copy() {
    try { await navigator.clipboard.writeText(text()); toast('Log copied'); } catch { download('capturetonkeli-log.txt', text(), 'text/plain'); }
  }
  redraw();
}
