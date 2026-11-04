// DOM helpers, banners, toast, dialogs and the error/message renderer.
import { describe } from '../errors.js';

/** h('div.cls#id', {attr, onclick}, ...children) */
export function h(spec, attrs = {}, ...kids) {
  const [tag, ...rest] = spec.split(/(?=[.#])/);
  const el = document.createElement(tag || 'div');
  for (const r of rest) { if (r[0] === '.') el.classList.add(r.slice(1)); else el.id = r.slice(1); }
  if (attrs && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) { kids.unshift(attrs); attrs = {}; }
  for (const [k, v] of Object.entries(attrs ?? {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className = v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k in el && k !== 'list' && k !== 'form') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of kids.flat(Infinity)) if (c !== null && c !== undefined && c !== false) el.append(c instanceof Node ? c : String(c));
  return el;
}
export const $ = (sel, root = document) => root.querySelector(sel);

/** Renders a description ({title,text,actions,detail,level}) as an element. */
export function messageEl(d, cls = 'inline-msg', buttons = []) {
  return h(`div.${cls}.${d.level ?? 'error'}`, { role: d.level === 'error' ? 'alert' : 'status' },
    h('div.b-main',
      h('div.b-title', d.title),
      d.text && h('div', d.text),
      d.actions?.length ? h('ul', d.actions.map((a) => h('li', a))) : null,
      d.detail && h('div.b-detail', d.detail)),
    buttons.length ? h('div.b-buttons', buttons) : null);
}

/** Inline message under a control: show(container, err|desc, context) – clears with show(container, null). */
export function inline(container, errOrDesc, context = '') {
  container.replaceChildren();
  if (!errOrDesc) return null;
  const d = errOrDesc.title ? errOrDesc : describe(errOrDesc, context);
  const el = messageEl(d);
  container.append(el);
  return d;
}

const banners = new Map();
/** Top banner for link-level problems; key dedups; returns the element. */
export function banner(key, d, buttons = []) {
  clearBanner(key);
  const close = h('button.icon', { 'aria-label': 'Dismiss', onclick: () => clearBanner(key) }, '✕');
  const el = messageEl(d, 'banner', [...buttons, close]);
  banners.set(key, el);
  $('#banners').append(el);
  return el;
}
export function clearBanner(key) { banners.get(key)?.remove(); banners.delete(key); }

let toastT;
export function toast(text, ms = 2500) {
  const t = $('#toast');
  t.textContent = text;
  t.classList.remove('hidden');
  clearTimeout(toastT);
  toastT = setTimeout(() => t.classList.add('hidden'), ms);
}

/** Modal dialog; buttons: [{label, value, primary, danger}]; resolves the clicked value ('' on Esc). */
export function dialog(title, body, buttons = [{ label: 'OK', value: 'ok', primary: true }]) {
  const dlg = $('#dlg');
  $('#dlg-title').textContent = title;
  $('#dlg-body').replaceChildren(...[].concat(body).map((b) => (b instanceof Node ? b : h('p', b))));
  $('#dlg-buttons').replaceChildren(...buttons.map((b) => h(`button${b.primary ? '.primary' : ''}${b.danger ? '.danger' : ''}`, { value: b.value, autofocus: !!b.primary }, b.label)));
  return new Promise((resolve) => {
    dlg.addEventListener('close', () => resolve(dlg.returnValue), { once: true });
    dlg.returnValue = '';
    dlg.showModal();
  });
}
export const confirmDlg = async (title, text, okLabel = 'OK', danger = false) =>
  (await dialog(title, text, [{ label: 'Cancel', value: 'cancel' }, { label: okLabel, value: 'ok', primary: !danger, danger }])) === 'ok';

export function download(name, data, type = 'application/octet-stream') {
  const url = URL.createObjectURL(data instanceof Blob ? data : new Blob([data], { type }));
  const a = h('a', { href: url, download: name });
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10000);
}

export const fmtNum = (n) => (n ?? 0).toLocaleString('en-US');
export const fmtBytes = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(2)} MB` : n >= 1e3 ? `${(n / 1e3).toFixed(1)} kB` : `${Math.round(n)} B`);
