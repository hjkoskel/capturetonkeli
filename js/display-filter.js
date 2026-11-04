// Display filter language (docs/04 §8), evaluated host-side in the worker:
//   type==beacon   ssid~"home"   rssi>-60   src==aa:bb:cc:dd:ee:ff   ch==6
//   len>200   fcs_err   combined with and/or/not/&&/||/! and parentheses.
// compileDisplayFilter(expr) → fn(packet) or throws Error with a position hint.
import * as D from './dot11.js';
import { PKT, bbName } from './wire.js';

const norm = (s) => String(s).toLowerCase().replace(/[\s_\-+]/g, '');
const ALIAS = { probereq: 'proberequest', proberesp: 'proberesponse', qos: 'qosdata', ba: 'blockack', bar: 'blockackrequest',
  assocreq: 'assocrequest', assocresp: 'assocresponse', deauthentication: 'deauth', disassociation: 'disassoc' };

/** Numeric/string field getters on {meta, data}; dot11 parsed lazily once per packet. */
const FIELDS = {
  ch: (p) => p.meta.channel, channel: (p) => p.meta.channel, rssi: (p) => p.meta.rssi, noise: (p) => p.meta.noise,
  len: (p) => p.meta.origLen, caplen: (p) => p.meta.capLen, rate: (p) => p.meta.rate, bb: (p) => norm(bbName(p.meta.bbFormat)),
  type: (p) => norm(dot(p).name), class: (p) => D.TYPE_CLASS[dot(p).type],
  ssid: (p) => dot(p).ssid ?? '', src: (p) => dot(p).addr2 ?? '', dst: (p) => dot(p).addr1 ?? '', bssid: (p) => dot(p).bssid ?? '',
  addr: (p) => [dot(p).addr1, dot(p).addr2, dot(p).addr3].filter(Boolean).join(' '), seq: (p) => dot(p).seq ?? -1,
};
const FLAGS = {
  fcs_err: (p) => !!(p.meta.flags & PKT.FCS_ERR), truncated: (p) => !!(p.meta.flags & PKT.TRUNCATED),
  ht40: (p) => !!(p.meta.flags & PKT.HT40), retry: (p) => !!dot(p).retry, protected: (p) => !!dot(p).protected,
  mgmt: (p) => dot(p).type === 0, ctrl: (p) => dot(p).type === 1, data: (p) => dot(p).type === 2,
  beacon: (p) => norm(dot(p).name) === 'beacon',
};

function dot(p) { return p._d ?? (p._d = D.parse(p.data)); }

function tokenize(s) {
  const toks = [];
  const re = /\s*(?:(\(|\))|(==|!=|<=|>=|<|>|~|&&|\|\||!)|"((?:[^"\\]|\\.)*)"|([^\s()<>=!~"&|]+))/y;
  let m;
  re.lastIndex = 0;
  while (re.lastIndex < s.length) {
    const at = re.lastIndex;
    if (/^\s*$/.test(s.slice(at))) break;
    m = re.exec(s);
    if (!m) throw new Error(`unexpected character at ${at + 1}: "${s.slice(at, at + 8)}"`);
    if (m[1]) toks.push({ k: m[1], at });
    else if (m[2]) toks.push({ k: m[2], at });
    else if (m[3] !== undefined) toks.push({ k: 'str', v: m[3].replace(/\\(.)/g, '$1'), at });
    else {
      const w = m[4], lw = w.toLowerCase();
      if (lw === 'and') toks.push({ k: '&&', at }); else if (lw === 'or') toks.push({ k: '||', at }); else if (lw === 'not') toks.push({ k: '!', at });
      else toks.push({ k: 'word', v: w, at });
    }
  }
  return toks;
}

export function compileDisplayFilter(src) {
  const toks = tokenize(src);
  let i = 0;
  const peek = () => toks[i], take = () => toks[i++];
  const fail = (msg, t = peek()) => { throw new Error(`${msg}${t ? ` at position ${t.at + 1}` : ' at the end'}`); };

  function orE() { let l = andE(); while (peek()?.k === '||') { take(); const r = andE(), a = l; l = (p) => a(p) || r(p); } return l; }
  function andE() { let l = notE(); while (peek()?.k === '&&') { take(); const r = notE(), a = l; l = (p) => a(p) && r(p); } return l; }
  function notE() { if (peek()?.k === '!') { take(); const e = notE(); return (p) => !e(p); } return prim(); }
  function prim() {
    const t = take();
    if (!t) fail('expression expected');
    if (t.k === '(') { const e = orE(); if (take()?.k !== ')') fail('")" expected'); return e; }
    if (t.k !== 'word') fail(`unexpected "${t.k}"`, t);
    const name = t.v.toLowerCase();
    const op = peek();
    if (!op || !['==', '!=', '<', '>', '<=', '>=', '~'].includes(op.k)) {
      if (FLAGS[name]) return FLAGS[name];
      if (FIELDS[name]) fail(`"${name}" needs a comparison, e.g. ${name}==…`, op);
      fail(`unknown field "${t.v}" (known: ${[...Object.keys(FIELDS), ...Object.keys(FLAGS)].join(', ')})`, t);
    }
    take();
    const get = FIELDS[name];
    if (!get) fail(`unknown field "${t.v}"`, t);
    const vt = take();
    if (!vt || (vt.k !== 'word' && vt.k !== 'str')) fail('value expected', vt);
    let v = vt.v;
    const numeric = ['ch', 'channel', 'rssi', 'noise', 'len', 'caplen', 'rate', 'seq'].includes(name);
    if (numeric) {
      v = Number(v);
      if (!Number.isFinite(v)) fail(`"${vt.v}" is not a number`, vt);
    } else if (name === 'type') {
      v = ALIAS[norm(v)] ?? norm(v);
    } else if (['src', 'dst', 'bssid', 'addr'].includes(name)) {
      v = v.toLowerCase().replace(/-/g, ':');
    } else if (name !== 'ssid') v = norm(v);
    const cmp = {
      '==': (a) => a === v, '!=': (a) => a !== v, '<': (a) => a < v, '>': (a) => a > v, '<=': (a) => a <= v, '>=': (a) => a >= v,
      '~': (a) => String(a).toLowerCase().includes(String(v).toLowerCase()),
    }[op.k];
    if (!numeric && !['==', '!=', '~'].includes(op.k)) fail(`"${op.k}" only works on numbers`, op);
    if (name === 'addr' && op.k === '==') return (p) => get(p).split(' ').includes(v);
    return (p) => cmp(get(p));
  }

  const e = orE();
  if (i < toks.length) fail(`unexpected "${toks[i].v ?? toks[i].k}"`);
  return e;
}
