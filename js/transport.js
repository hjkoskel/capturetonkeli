// Transports (docs/01, docs/04 §1–2): one interface, three backends.
//   open() · close() · write(u8) · onData(cb) · onLost(cb(err)) · label
// Control lines (DTR/RTS) are never changed: lowering RTS wedged the C5 ROM
// and the OS default does not reset the app (docs/01 risk #1, M3).
import { DemoDevice, DemoAir } from './demo-device.js';
import { chooseTransport, androidSerialState, platform } from './capabilities.js';

export const USB_FILTER = { usbVendorId: 0x303a, usbProductId: 0x1001 };

class Base {
  constructor() { this.dataCb = () => {}; this.lostCb = () => {}; this.closed = true; this.overruns = 0; }
  onData(cb) { this.dataCb = cb; }
  onLost(cb) { this.lostCb = cb; }
}

// ---- Web Serial (desktop) -----------------------------------------------------------

export class SerialTransport extends Base {
  /** port: a SerialPort (from requestPort or getPorts). */
  constructor(port) {
    super();
    this.port = port;
    const i = port.getInfo?.() ?? {};
    this.kind = 'serial';
    this.label = `Web Serial ${i.usbVendorId === 0x303a ? '(Espressif USB-Serial/JTAG)' : ''}`.trim();
  }

  get portId() { return this.port; }

  static async request() {
    if (!navigator.serial) throw Object.assign(new Error('Web Serial is not available'), { name: 'NotSupportedError' });
    return new SerialTransport(await navigator.serial.requestPort({ filters: [USB_FILTER] }));
  }

  static async known() {
    if (!navigator.serial) return [];
    return (await navigator.serial.getPorts()).filter((p) => p.getInfo().usbProductId === 0x1001).map((p) => new SerialTransport(p));
  }

  async open() {
    await this.port.open({ baudRate: 115200, bufferSize: 1 << 20 });
    this.closed = false;
    this.onDisconnect = (e) => { if (e.target === this.port || e.port === this.port) this.fail(Object.assign(new Error('The device was disconnected'), { name: 'NetworkError' })); };
    navigator.serial.addEventListener('disconnect', this.onDisconnect);
    this.readLoop();
  }

  async readLoop() {
    while (!this.closed && this.port.readable) {
      const reader = this.port.readable.getReader();
      this.reader = reader;
      try {
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          if (value?.length) this.dataCb(value);
        }
      } catch (e) {
        if (this.closed) break;
        if (e.name === 'BufferOverrunError' || e.name === 'BreakError' || e.name === 'FramingError' || e.name === 'ParityError') {
          // recoverable: port.readable is replaced; the decoder resyncs
          this.overruns++;
          this.dataCb(null, e);
          continue;
        }
        this.fail(e);
        break;
      } finally {
        try { reader.releaseLock(); } catch { /* already released */ }
      }
    }
    if (!this.closed) this.fail(Object.assign(new Error('The serial stream ended'), { name: 'NetworkError' }));
  }

  async write(u8) {
    if (this.closed || !this.port.writable) throw Object.assign(new Error('Port is not open'), { name: 'InvalidStateError' });
    const w = this.port.writable.getWriter();
    try { await w.write(u8); } finally { w.releaseLock(); }
  }

  fail(err) {
    if (this.closed) return;
    this.close();
    this.lostCb(err);
  }

  async close() {
    if (this.closed) return;
    this.closed = true;
    navigator.serial?.removeEventListener('disconnect', this.onDisconnect);
    try { await this.reader?.cancel(); } catch { /* ignore */ }
    try { await this.port.close(); } catch { /* ignore */ }
  }
}

// ---- WebUSB + CDC-ACM shim (Android Chrome) ---------------------------------------------

export class UsbTransport extends Base {
  constructor(device) {
    super();
    this.dev = device;
    this.kind = 'webusb';
    this.label = 'WebUSB (CDC-ACM)';
  }

  get portId() { return this.dev; }

  static async request() {
    if (!navigator.usb) throw Object.assign(new Error('WebUSB is not available'), { name: 'NotSupportedError' });
    return new UsbTransport(await navigator.usb.requestDevice({ filters: [{ vendorId: 0x303a, productId: 0x1001 }] }));
  }

  static async known() {
    if (!navigator.usb) return [];
    return (await navigator.usb.getDevices()).filter((d) => d.vendorId === 0x303a && d.productId === 0x1001).map((d) => new UsbTransport(d));
  }

  async open() {
    const d = this.dev;
    await d.open();
    if (!d.configuration) await d.selectConfiguration(1);
    const ifs = d.configuration.interfaces;
    const comm = ifs.find((i) => i.alternates.some((a) => a.interfaceClass === 0x02));
    const data = ifs.find((i) => i.alternates.some((a) => a.interfaceClass === 0x0a));
    if (!comm || !data) throw new Error('This USB device has no CDC-ACM interface (not an ESP32 USB-Serial/JTAG?)');
    try {
      await d.claimInterface(comm.interfaceNumber);
      await d.claimInterface(data.interfaceNumber);
    } catch (e) {
      throw new Error(`Unable to claim the USB interface: ${e.message}`);
    }
    const alt = data.alternates.find((a) => a.interfaceClass === 0x0a);
    this.epIn = alt.endpoints.find((e) => e.direction === 'in').endpointNumber;
    this.epOut = alt.endpoints.find((e) => e.direction === 'out').endpointNumber;
    this.commIf = comm.interfaceNumber;
    // SET_LINE_CODING 115200 8N1 (ignored by USJ, expected by CDC hosts)
    await d.controlTransferOut({ requestType: 'class', recipient: 'interface', request: 0x20, value: 0, index: this.commIf },
      Uint8Array.of(0x00, 0xc2, 0x01, 0x00, 0, 0, 8));
    // SET_CONTROL_LINE_STATE: DTR|RTS = the OS default state (does not reset the app)
    await d.controlTransferOut({ requestType: 'class', recipient: 'interface', request: 0x22, value: 0x03, index: this.commIf });
    this.closed = false;
    this.onDisconnect = (e) => { if (e.device === d) this.fail(Object.assign(new Error('The device was disconnected'), { name: 'NetworkError' })); };
    navigator.usb.addEventListener('disconnect', this.onDisconnect);
    for (let i = 0; i < 8; i++) this.inLoop(); // ≥ 8 transfers in flight (docs/01)
  }

  async inLoop() {
    while (!this.closed) {
      try {
        const r = await this.dev.transferIn(this.epIn, 16384);
        if (r.status === 'stall') { await this.dev.clearHalt('in', this.epIn); continue; }
        if (r.data?.byteLength) this.dataCb(new Uint8Array(r.data.buffer, r.data.byteOffset, r.data.byteLength));
      } catch (e) {
        if (!this.closed) this.fail(e);
        return;
      }
    }
  }

  async write(u8) {
    if (this.closed) throw Object.assign(new Error('Device is not open'), { name: 'InvalidStateError' });
    await this.dev.transferOut(this.epOut, u8);
  }

  fail(err) { if (this.closed) return; this.close(); this.lostCb(err); }

  async close() {
    if (this.closed) return;
    this.closed = true;
    navigator.usb?.removeEventListener('disconnect', this.onDisconnect);
    try { await this.dev.close(); } catch { /* ignore */ }
  }
}

// ---- demo (simulated dongle in this page) ------------------------------------------------

const demoDevices = new Map(); // index → DemoDevice; persists across reconnects, like a real dongle
let demoAir = null;           // the simulated air all demo dongles hear (docs/14 diversity demo)

export class DemoTransport extends Base {
  /** index: which demo dongle (0 = the usual one; 1–3 = more antennas for diversity). */
  constructor(index = 0) {
    super();
    this.kind = 'demo';
    this.index = index;
    this.label = index ? `Demo dongle ${index + 1} (simulated)` : 'Demo dongle (simulated)';
    demoAir ??= new DemoAir(Date.now() & 0xffff);
    let d = demoDevices.get(index);
    if (!d) {
      d = new DemoDevice({ onBytes: (u8) => DemoTransport.current.get(index)?.deliver(u8), seed: (Date.now() & 0xffff) + index, index, air: demoAir });
      demoDevices.set(index, d);
    }
    this.device = d;
  }
  /** The port this transport talks to (two members must not share one). */
  get portId() { return `demo:${this.index}`; }
  async open() {
    DemoTransport.current.set(this.index, this);
    this.closed = false;
    this.device.attach();
  }
  deliver(u8) {
    if (this.closed) return;
    if (this.overrunNext) { this.overrunNext = false; this.overruns++; this.dataCb(null, Object.assign(new Error('simulated overrun'), { name: 'BufferOverrunError' })); return; }
    this.dataCb(u8.slice());
  }
  async write(u8) {
    if (this.closed) throw Object.assign(new Error('Port is not open'), { name: 'InvalidStateError' });
    queueMicrotask(() => this.device.write(u8));
  }
  /** Fault injection: simulate a host overrun / unplug. */
  simulateOverrun() { this.overrunNext = true; }
  simulateUnplug() { this.close(); this.lostCb(Object.assign(new Error('The device was disconnected (simulated)'), { name: 'NetworkError' })); }
  async close() {
    if (this.closed) return;
    this.closed = true;
    this.device.detach();
    if (DemoTransport.current.get(this.index) === this) DemoTransport.current.delete(this.index);
  }
}
DemoTransport.current = new Map();

/** auto: Web Serial on desktop; on Android Web Serial once it has shown a USB port, else WebUSB (docs/04 §1, js/capabilities.js). */
export function pickTransport(pref = 'auto') {
  const p = platform();
  const { api } = chooseTransport(pref, navigator, p.android ? androidSerialState(p) : 'untested');
  return api === 'serial' ? SerialTransport : api === 'webusb' ? UsbTransport : null;
}
