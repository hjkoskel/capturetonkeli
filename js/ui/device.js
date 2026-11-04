// Device tab (docs/04 §11): HELLO info, CONFIG readback, re-push, reboot,
// firmware section, demo fault injection.
import { app } from '../app.js';
import { h, inline, confirmDlg, toast } from './dom.js';
import * as W from '../wire.js';
import * as semver from '../semver.js';
import { DemoTransport } from '../transport.js';
import * as flasher from '../flasher.js';

export function initDevice(root) {
  const info = h('dl.kv'), cfg = h('dl.kv'), fw = h('div'), msg = h('div');
  const faults = h('div');
  root.append(
    h('h3', 'Dongle'), info,
    h('h3', 'Configuration (readback)'), cfg,
    h('div.row', h('button', { onclick: reread }, 'Re-read config'), h('button', { onclick: repush }, 'Re-push my config')),
    h('h3', 'Firmware'), fw,
    h('h3', 'Reboot'),
    h('div.row', h('button', { onclick: () => reboot(0) }, 'Reboot dongle'), h('button', { onclick: () => reboot(1) }, 'Reboot to download mode')),
    msg,
    faults,
  );

  function drawInfo() {
    const i = app.info;
    if (!i) { info.replaceChildren(h('dt', 'State'), h('dd', 'not connected')); fw.replaceChildren(fwButtons()); return; }
    const up = () => (app.devStats ? app.devStats.deviceTimeUs : i.deviceTimeUs) / 1e6;
    const flags = [i.fwFlags & W.FW_FLAG.DIRTY && 'dirty', i.fwFlags & W.FW_FLAG.DEV && 'dev (untagged)', i.fwFlags & W.FW_FLAG.IDF_MISMATCH && 'IDF ≠ pinned'].filter(Boolean);
    const kv = [['Transport', app.transport?.label ?? ''], ['Firmware', `${W.fwString(i.fw)}${flags.length ? ` (${flags.join(', ')})` : ''}`],
      ['Build ID', Array.from(i.buildId, (b) => b.toString(16).padStart(2, '0')).join('')], ['Protocol', `v${i.protoVer}`], ['MAC', W.macString(i.mac)],
      ['Capabilities', W.capNames(i.caps).join(', ') || 'none'], ['Max frame / rules', `${i.maxFrame} B / ${i.maxRules}`],
      ['Uptime', `${up().toFixed(0)} s`], ['Last reset', W.resetName(i.resetReason)], ['Safe mode', i.stateFlags & W.STATE_SAFE_MODE ? 'YES – radio off' : 'no']];
    info.replaceChildren(...kv.flatMap(([k, v]) => [h('dt', k), h('dd', String(v))]));
    // firmware section (docs/04 §4.1, §11.1)
    const host = app.hostFw;
    const lines = [];
    if (!host) lines.push(h('p.small.muted', 'This page has no bundled firmware (development build), so the version check is off. Release pages carry fw/manifest.json.'));
    else if (!semver.compatible(host, i.fw)) lines.push(h('div.inline-msg.warn', `Firmware ${W.fwString(i.fw)} is not compatible with this page (${host.major}.${host.minor}.x).`));
    else if (semver.compare(host, i.fw) !== 0) lines.push(h('div.inline-msg.info', `Firmware ${W.fwString(i.fw)}, page bundles ${semver.toString(host)} – compatible.`));
    else lines.push(h('div.inline-msg.ok', `Firmware matches this page (${semver.toString(host)}).`));
    fw.replaceChildren(...lines, fwButtons());
  }

  function fwButtons() {
    const file = h('input.hidden', { type: 'file', multiple: true, accept: '.bin,.json', onchange: (e) => { const f = [...e.target.files]; e.target.value = ''; if (f.length) flasher.flashFromFiles(f); } });
    const real = app.transport && app.transport.kind !== 'demo';
    return h('div',
      h('div.row',
        app.manifest ? h('button', { onclick: () => flasher.updateToBundled(), disabled: !real, title: real ? '' : 'Connect a real dongle first' }, `Update to bundled ${app.manifest.version}`) : null,
        h('button', { onclick: () => flasher.flashBare(), disabled: !app.manifest }, 'Flash firmware to a new dongle…'),
        h('button', { onclick: () => file.click() }, 'Flash from file…'), file),
      h('p.small.muted', `Only changed 4 KB sectors are written (MD5-compared). Sectors written this session: ${app.sectorsWritten ?? 0}. eFuses are never touched.`));
  }

  function drawCfg() {
    const c = app.config;
    if (!c) { cfg.replaceChildren(); return; }
    const hex = (n) => `0x${(n >>> 0).toString(16).padStart(8, '0')}`;
    const kv = [['cfg_gen', c.cfgGen], ['Channel', `${c.channel} ${W.bwName(c.bw)} (${c.band ? '5' : '2.4'} GHz)`],
      ['Country', `${c.country.cc} mode ${c.country.mode} (${c.country.schan}+${c.country.nchan})`], ['Snaplen', c.snaplen || 'unlimited'],
      ['Driver masks', `${hex(c.typeMask)} / ${hex(c.ctrlMask)}`], ['Capture', c.captureOn ? 'on' : 'off'],
      ['Filter', `${c.nRules} rules, crc ${hex(c.rulesCrc32)}`]];
    cfg.replaceChildren(...kv.flatMap(([k, v]) => [h('dt', k), h('dd', String(v))]));
  }

  async function reread() { inline(msg, await app.act('Re-read config', (c) => c.getConfig())); }
  async function repush() {
    inline(msg, await app.act('Re-push config', async () => { await app.pushDesired(); }));
    if (!msg.firstChild) toast('Configuration re-sent');
  }
  async function reboot(mode) {
    const ok = await confirmDlg(mode ? 'Reboot to download mode?' : 'Reboot the dongle?',
      mode ? 'The dongle stops capturing and waits for a flasher (ctpbench fw flash, esptool --before no-reset). Unplug it to leave download mode.'
        : 'Capture stops briefly; the page reconnects and restores your configuration.', 'Reboot', true);
    if (!ok) return;
    inline(msg, await app.act('Reboot', (c) => c.reboot(mode)));
    if (mode) { await app.disconnect(true); toast('Dongle is in download mode'); }
  }

  function drawFaults() {
    const t = app.transport;
    if (!(t instanceof DemoTransport)) { faults.replaceChildren(); return; }
    const d = t.device;
    const tog = (label, fn) => h('button.small', { onclick: fn }, label);
    faults.replaceChildren(
      h('h3', 'Demo fault injection'),
      h('p.small.muted', 'Exercises the recovery paths of the page (docs/04 §13).'),
      h('div.row',
        tog('Drop bytes 1%', () => { d.faults.drop = d.faults.drop ? 0 : 0.01; toast(`drop ${d.faults.drop ? 'on' : 'off'}`); }),
        tog('Corrupt bytes 1%', () => { d.faults.corrupt = d.faults.corrupt ? 0 : 0.01; toast(`corrupt ${d.faults.corrupt ? 'on' : 'off'}`); }),
        tog('Inject garbage', () => d.garbage()),
        tog('Stall 3 s', () => d.stall(3000)),
        tog('Reboot dongle', () => d.reboot()),
        tog('Host overrun', () => t.simulateOverrun()),
        tog('Unplug', () => t.simulateUnplug()),
        tog('Saturate (drop_ring)', () => { d.faults.saturate = !d.faults.saturate; toast(`saturate ${d.faults.saturate ? 'on' : 'off'}`); })));
  }

  app.addEventListener('info', () => { drawInfo(); drawFaults(); });
  app.addEventListener('config', drawCfg);
  app.addEventListener('stats', drawInfo);
  app.addEventListener('conn', (e) => { if (e.detail === 'disconnected') { drawInfo(); drawCfg(); drawFaults(); } });
  drawInfo();
}
