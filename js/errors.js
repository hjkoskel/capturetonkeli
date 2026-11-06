// One place that turns any failure into a message a person can act on
// (docs/04 §12). describe() never throws and always returns
// {title, text, actions: string[], level: 'error'|'warn'|'info', detail}.
import * as W from './wire.js';

const DEVICE = {
  [W.ERR.UNKNOWN_TYPE]: ['The dongle does not know this command', 'The dongle firmware is older or newer than this page.', ['Update the dongle firmware (Device tab).']],
  [W.ERR.BAD_BODY]: ['The dongle rejected a malformed command', 'This is a bug in the page or firmware.', ['Copy the log (Log tab) and report it.']],
  [W.ERR.INVALID_ARG]: ['Invalid value', 'The dongle rejected one of the values.', ['Check the value you entered.']],
  [W.ERR.CHANNEL_NOT_ALLOWED]: ['Channel not allowed by the country table', 'The radio driver only tunes to channels in the active country table. Receiving is not restricted by law, but the driver still enforces the table.',
    ['Pick a country that includes this channel (Radio → Country), e.g. JP for channel 14 or FI/US for higher 5 GHz channels.', 'Or use the manual range for 2.4 GHz.']],
  [W.ERR.COUNTRY_INVALID]: ['Country code not accepted', 'The ESP-IDF radio driver supports only a fixed list of country codes.', ['Choose a code from the list, or "01" (world).']],
  [W.ERR.NOT_SUPPORTED]: ['Not supported by this dongle', 'The firmware does not offer this feature (see Device → capabilities).', ['HT40 is not available on the ESP32-C5 in receive-only mode yet; use HT20.']],
  [W.ERR.FILTER_INVALID]: ['Filter rejected by the dongle', 'The encoded filter breaks a limit (rules, terms, term length).', ['Check the rule editor for red fields.']],
  [W.ERR.NO_SESSION]: ['The dongle forgot this session', 'The dongle stopped capture because it did not hear from the page for 2 s (tab in background, cable glitch).', ['The page reconnects automatically; press Capture again if it does not.']],
  [W.ERR.RADIO]: ['The radio driver refused the change', 'ESP-IDF returned an error for this setting.', ['Try another value; reboot the dongle (Device tab) if it persists.']],
  [W.ERR.SAFE_MODE]: ['Dongle is in safe mode', 'The dongle crashed 3 times within a minute, so it keeps the radio off. Only status commands work.', ['Reboot it (Device tab) or unplug it, then update the firmware.', 'Check the Log for the reset reason.']],
};

const DOMEX = {
  NotFoundError: ['No dongle selected', 'The device picker was closed without choosing a port.', ['Click Connect and pick "USB JTAG/serial debug unit" (Espressif).',
    'If nothing is listed: check the USB cable carries data (some cables are charge-only) and that the dongle is plugged in.']],
  SecurityError: ['The browser blocked access to USB', 'Web Serial/WebUSB only work on https:// pages or http://localhost, and can be disabled by policy.',
    ['Open the page over https:// (GitHub Pages) or http://localhost (make serve).', 'Check the site permissions in the address bar.']],
  NetworkError: ['The port is in use by another program', 'Another program has the dongle open (e.g. a serial monitor, idf.py monitor, ctpbench, another browser tab).',
    ['Close the other program or tab, then click Connect again.', 'On Linux, ModemManager can grab new serial ports: disable it or add a udev rule.']],
  InvalidStateError: ['The port is already open', 'This page or another tab already has the port open.', ['Close other tabs using the dongle, or reload this page.']],
  BufferOverrunError: ['The browser could not keep up with the data', 'Received bytes were lost because the page was too busy (heavy tab, background throttling).',
    ['The page reconnects automatically.', 'Lower the snaplen or tighten the filter to reduce the data rate.']],
  BreakError: ['Serial break received', 'The USB serial line reported a break condition.', ['The page reconnects automatically.']],
  NotAllowedError: ['Permission to the USB device was denied', 'The browser or the operating system refused access.',
    ['Allow the device in the browser prompt.', 'On Android: accept the USB permission dialog; on Linux: add your user to the "uucp"/"dialout" group.']],
  AbortError: ['The operation was cancelled', '', []],
  NotSupportedError: ['Not supported by this browser', '', ['Use Chrome, Edge or Opera on desktop, Firefox ≥ 151, or Chrome on Android.']],
};

/**
 * @param {any} err  CtpError | DOMException | Error | string
 * @param {string} context  what the user tried ("Connect", "Set channel 13", …)
 */
export function describe(err, context = '') {
  try {
    return describeInner(err, context);
  } catch (e) {
    return { title: `${context || 'Error'}`, text: String(err?.message ?? err), actions: [], level: 'error', detail: String(e) };
  }
}

function describeInner(err, context) {
  const pre = context ? `${context}: ` : '';
  if (err?.name === 'CtpError') {
    switch (err.kind) {
      case 'device': {
        const [title, text, actions] = DEVICE[err.code] ?? ['The dongle rejected the command', '', []];
        const esp = W.espErrName(err.espErr);
        return { title: pre + title, text, actions, level: 'error',
          detail: [W.errName(err.code), esp, err.detail && `"${err.detail}"`].filter(Boolean).join(' · ') };
      }
      case 'timeout':
        return { title: pre + 'The dongle did not answer', level: 'error',
          text: 'Requests were retried 4 times without a reply. The firmware may be busy, crashed, or not CTP firmware at all.',
          actions: ['Wait a moment: the page keeps trying.', 'Unplug and replug the dongle.', 'If it never answers: flash the firmware (Device → Flash firmware).'], detail: err.message };
      case 'closed':
        return { title: pre + 'Connection closed', level: 'warn', text: 'The link to the dongle was closed (unplugged, reset, or another error).',
          actions: ['Plug the dongle in and click Connect.'], detail: err.message };
      case 'protocol':
        return { title: pre + 'Unexpected answer from the device', level: 'error',
          text: 'The device on this port does not behave like a capturetonkeli dongle (another ESP32 board or firmware?).',
          actions: ['Make sure you picked the right port.', 'Flash the capturetonkeli firmware (Device → Flash firmware).'], detail: err.message };
      case 'version':
        return { title: pre + (err.protocol ? 'Protocol version mismatch' : 'Firmware version mismatch'), level: 'warn',
          text: err.protocol ? 'This page cannot talk to the firmware on the dongle at all.' : 'The dongle runs a firmware this page was not tested with.',
          actions: ['Update the dongle firmware to the version bundled with this page.'], detail: err.message };
      default: break;
    }
  }
  const name = err?.name ?? '';
  const msg0 = String(err?.message ?? '');
  if (/user gesture|user activation/i.test(msg0)) {
    return { title: pre + 'The browser needs a click to open this dialog', level: 'warn',
      text: 'Device and file pickers only open in direct response to a click or key press (not after a delay or from a background task).',
      actions: ['Click the button again.'], detail: `${name}: ${msg0}` };
  }
  if (/showSaveFilePicker|file picker|getDirectory|createWritable|QuotaExceeded/i.test(msg0) || name === 'QuotaExceededError') {
    return { title: pre + 'Could not open the file for writing', level: 'error',
      text: name === 'QuotaExceededError' ? 'The browser storage is full.' : 'The browser refused access to the file system.',
      actions: ['Choose another location, free some storage, or use "Export buffer" from the menu instead.'], detail: `${name}: ${msg0}` };
  }
  if (/disconnected|device (has been )?lost|stream ended/i.test(msg0)) {
    return { title: pre + 'Dongle disconnected', level: 'warn', text: 'The USB device went away (unplugged, reset or re-enumerated).',
      actions: ['Plug it back in; the page reconnects to a known dongle automatically.'], detail: `${name}: ${msg0}` };
  }
  if (DOMEX[name]) {
    const [title, text, actions] = DOMEX[name];
    return { title: pre + title, text, actions, level: name === 'NotFoundError' || name === 'AbortError' ? 'info' : 'error', detail: `${name}: ${err.message ?? ''}` };
  }
  if (name === 'FilterError') return { title: pre + 'Filter is not valid', text: err.message, actions: ['Fix the highlighted rule.'], level: 'error', detail: '' };
  if (name === 'ShortBody') return { title: pre + 'Malformed reply from the dongle', text: err.message, actions: ['Update the firmware or this page.'], level: 'error', detail: '' };
  // USB-specific messages (WebUSB on desktop: an OS driver owns the interface)
  const msg = String(err?.message ?? err ?? 'unknown error');
  if (/claimInterface|Unable to claim|interface.*busy/i.test(msg)) {
    return { title: pre + 'The operating system holds the USB interface', level: 'error',
      text: 'On desktop, the OS serial driver owns the dongle, so WebUSB cannot use it.',
      actions: ['Use the Web Serial transport (Settings → Transport: auto).', 'On Android this works because Chrome detaches the kernel driver.'], detail: msg };
  }
  if (/device (has been )?(lost|disconnected)|The device was disconnected|NetworkError/i.test(msg)) {
    return { title: pre + 'Dongle disconnected', level: 'warn', text: 'The USB device went away (unplugged, reset or re-enumerated).',
      actions: ['Plug it back in; the page reconnects to a known dongle automatically.'], detail: msg };
  }
  return { title: pre + 'Something went wrong', level: 'error', text: msg,
    actions: ['Retry. If it keeps happening, copy the log (Log tab → Copy all) and report it.'], detail: err?.stack ?? '' };
}

/** Browser support check (docs/04 §1); returns null if fine, else a description. */
export function unsupportedReason(env = globalThis) {
  if (!env.isSecureContext) {
    return { title: 'This page must be opened over https:// or http://localhost', level: 'error',
      text: 'Browsers only allow USB access from secure pages. file:// does not work.',
      actions: ['Open the published page (GitHub Pages) or run "make serve" and open http://localhost:8000.'] };
  }
  const nav = env.navigator ?? {};
  if (!nav.serial && !nav.usb) {
    const ua = nav.userAgent ?? '';
    const ios = /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && nav.maxTouchPoints > 1);
    return { title: 'This browser cannot talk to USB devices', level: 'error',
      text: ios ? 'iPhone and iPad browsers (all use WebKit) do not support Web Serial or WebUSB.'
        : /Firefox\//.test(ua) ? 'Your Firefox version has no Web Serial. Firefox ≥ 151 on desktop supports it.'
          : /Safari\//.test(ua) && !/Chrome\//.test(ua) ? 'Safari does not support Web Serial or WebUSB.' : 'This browser has neither Web Serial nor WebUSB.',
      actions: ['Use Chrome, Edge or Opera on Windows/macOS/Linux/ChromeOS, Firefox ≥ 151, or Chrome on Android (USB-OTG).',
        'You can still explore the page with "Try without hardware" (demo).'] };
  }
  return null;
}
