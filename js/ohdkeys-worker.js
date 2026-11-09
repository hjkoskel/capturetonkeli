// Short-lived worker: bind phrase → OpenHD keys (Argon2id, 2 × 64 MiB, a few seconds),
// so neither the page nor the packet worker stalls (docs/13 §10.5).
import { keysFromPhrase } from './ohdkeys.js';

self.onmessage = async (e) => {
  try {
    const keys = await keysFromPhrase(e.data.phrase, (p) => self.postMessage({ t: 'progress', p }));
    self.postMessage({ t: 'done', keys });
  } catch (err) {
    self.postMessage({ t: 'error', message: String(err?.message ?? err) });
  }
};
