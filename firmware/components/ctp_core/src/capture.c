/* Promiscuous-callback body (docs/07 §3): meta extraction, filter, snaplen,
 * push to ring. Budget ≤ 5 µs/frame; never blocks, no logging. */
#include <string.h>

#include "ctp/ctp.h"

static void bump(_Atomic uint32_t *v)
{
    atomic_fetch_add_explicit(v, 1u, memory_order_relaxed);
}

void ctp_capture_rx(ctp_t *c, const ctp_rx_t *rx)
{
    atomic_fetch_add_explicit(&c->cb_epoch, 1u, memory_order_acq_rel); /* odd: inside */
    if (!atomic_load_explicit(&c->capture_on, memory_order_acquire) ||
        !atomic_load_explicit(&c->session, memory_order_acquire)) {
        goto cleanup;
    }
    bump(&c->cnt.rx_total);
    atomic_store_explicit(&c->cnt.noise, rx->noise, memory_order_relaxed);

    const uint32_t slot = atomic_load_explicit(&c->active_filter, memory_order_acquire) & 1u;
    const ctp_filter_t *f = &c->filters[slot];
    const ctp_match_meta_t mm = {.fcs_err = rx->fcs_err, .rssi = rx->rssi, .orig_len = rx->len};
    if (!ctp_filter_match(f, rx->data, rx->len, &mm)) {
        if (ctp_filter_fcs_drop(f, &mm)) {
            bump(&c->cnt.drop_fcs);
        }
        goto cleanup;
    }
    bump(&c->cnt.rx_passed);
    if (atomic_exchange_explicit(&c->rxseq_reset, false, memory_order_acq_rel)) {
        c->rx_seq = 0u;
    }
    const uint16_t seq = c->rx_seq;
    c->rx_seq = (uint16_t)(c->rx_seq + 1u);

    uint32_t cap = rx->len;
    const uint32_t snap = atomic_load_explicit(&c->snaplen, memory_order_relaxed);
    if (snap != 0u && snap < cap) {
        cap = snap;
    }
    if (cap > CTP_MAX_CAP_LEN) {
        cap = CTP_MAX_CAP_LEN;
    }
    uint8_t *rec = ctp_ring_reserve(&c->ring, 1u + CTP_PKT_META_LEN + cap);
    if (rec == NULL) {
        bump(&c->cnt.drop_ring);
        goto cleanup;
    }
    uint8_t flags = 0u;
    flags = (uint8_t)(flags | (rx->fcs_err ? CTP_PKT_FCS_ERR : 0u));
    flags = (uint8_t)(flags | ((cap < rx->len) ? CTP_PKT_TRUNCATED : 0u));
    flags = (uint8_t)(flags | ((rx->channel > 14u) ? CTP_PKT_5GHZ : 0u));
    flags = (uint8_t)(flags | (rx->ht40 ? CTP_PKT_HT40 : 0u));
    flags = (uint8_t)(flags | (rx->ampdu ? CTP_PKT_AMPDU : 0u));
    flags = (uint8_t)(flags | (rx->rate_unknown ? CTP_PKT_RATE_UNKNOWN : 0u));
    const ctp_pkt_meta_t m = {.ts_us = rx->ts_us, .channel = rx->channel, .flags = flags, .rssi = rx->rssi,
                              .noise = rx->noise, .rate = rx->rate, .bb_format = rx->bb_format,
                              .orig_len = rx->len, .cap_len = (uint16_t)cap, .rx_seq = seq};
    rec[0] = (uint8_t)atomic_load_explicit(&c->cfg_gen, memory_order_acquire);
    (void)ctp_enc_pkt_meta(&rec[1], CTP_PKT_META_LEN, &m);
    if (cap > 0u) {
        memcpy(&rec[1u + CTP_PKT_META_LEN], rx->data, cap);
    }
    ctp_ring_commit(&c->ring);
    const uint32_t fill = ctp_ring_fill_pct(&c->ring);
    if (fill > atomic_load_explicit(&c->cnt.high_water, memory_order_relaxed)) {
        atomic_store_explicit(&c->cnt.high_water, fill, memory_order_relaxed);
    }
cleanup:
    atomic_fetch_add_explicit(&c->cb_epoch, 1u, memory_order_acq_rel); /* even: outside */
}
