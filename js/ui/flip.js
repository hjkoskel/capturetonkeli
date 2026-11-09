// Picture flip for cameras mounted upside down (FPV tab: hx viewer and OpenHD).
// A CSS transform on the canvas (css/app.css .flip-*), done by the compositor at
// display time: no per-frame pixel work, no extra latency, the OSD stays upright.
import * as settings from '../settings.js';
import { h } from './dom.js';

export const FLIPS = { none: 'no flip', rot180: 'upside down (rotate 180°)', mirror: 'mirror (left ↔ right)', vflip: 'flip vertically' };

/** A select that flips `canvas`, saved in settings[key]. Returns { el, value() }. */
export function flipControl(canvas, key) {
  const sel = h('select.small', { 'aria-label': 'Flip picture', title: 'For a camera mounted upside down. Display only: recordings keep the original picture.', onchange: () => { settings.update((x) => { x[key] = sel.value; }); apply(); } },
    Object.entries(FLIPS).map(([v, t]) => h('option', { value: v }, t)));
  const saved = settings.get()[key];
  sel.value = FLIPS[saved] ? saved : 'none';
  function apply() { for (const v of Object.keys(FLIPS)) canvas.classList.toggle(`flip-${v}`, sel.value === v && v !== 'none'); }
  apply();
  return { el: sel, value: () => sel.value };
}

/** `src` as shown on screen: itself when not flipped, else a flipped copy (for snapshots). */
export function flippedCanvas(src, flip) {
  if (flip === 'none') return src;
  const out = document.createElement('canvas');
  out.width = src.width; out.height = src.height;
  const c = out.getContext('2d'), x = flip !== 'vflip', y = flip !== 'mirror';
  c.setTransform(x ? -1 : 1, 0, 0, y ? -1 : 1, x ? src.width : 0, y ? src.height : 0);
  c.drawImage(src, 0, 0);
  return out;
}
