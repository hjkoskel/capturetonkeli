/* Protocol engine: dispatcher, req_id dedup cache, cfg_gen, host watchdog,
 * HELLO gating, response building, link-task framing (docs/03, docs/07 §3). */
#include <string.h>

#include "config.h"
#include "ctp/cobs.h"
#include "ctp/crc32.h"
#include "ctp/ctp.h"

/* ---- SPSC slot queue helpers ------------------------------------------------- */

static bool q_full(ctp_q_t *q, uint32_t n)
{
    const uint32_t h = atomic_load_explicit(&q->head, memory_order_relaxed);
    const uint32_t t = atomic_load_explicit(&q->tail, memory_order_acquire);
    return (h - t) >= n;
}

static bool q_empty(ctp_q_t *q)
{
    const uint32_t t = atomic_load_explicit(&q->tail, memory_order_relaxed);
    const uint32_t h = atomic_load_explicit(&q->head, memory_order_acquire);
    return h == t;
}

static uint32_t q_head(ctp_q_t *q, uint32_t n)
{
    return atomic_load_explicit(&q->head, memory_order_relaxed) % n;
}

static uint32_t q_tail(ctp_q_t *q, uint32_t n)
{
    return atomic_load_explicit(&q->tail, memory_order_relaxed) % n;
}

static void q_push(ctp_q_t *q)
{
    atomic_fetch_add_explicit(&q->head, 1u, memory_order_release);
}

static void q_pop(ctp_q_t *q)
{
    atomic_fetch_add_explicit(&q->tail, 1u, memory_order_release);
}

static uint32_t now_ms(const ctp_t *c)
{
    return (uint32_t)(c->port->now_us() / 1000u);
}

/* ---- ctrl → link output ------------------------------------------------------- */

/* Producer: ctrl task only. Waits briefly if the link is behind. */
static void out_push(ctp_t *c, uint8_t type, uint16_t req_id, const uint8_t *body, uint32_t len)
{
    CTP_ASSERT(len <= CTP_OUT_BODY_MAX);
    for (uint32_t i = 0u; q_full(&c->outq, CTP_OUT_SLOTS); i++) {
        if (i >= 50u) {
            return; /* link stalled > 50 ms: drop (host retries requests) */
        }
        c->port->delay_ms(1u);
    }
    ctp_out_slot_t *s = &c->out[q_head(&c->outq, CTP_OUT_SLOTS)];
    s->type = type;
    s->req_id = req_id;
    s->cfg_gen = c->cfg.cfg_gen;
    s->len = (uint16_t)len;
    if (len > 0u) {
        memmove(s->body, body, len); /* body may live in the same ctp_t (dedup cache) */
    }
    q_push(&c->outq);
}

void ctp_log(ctp_t *c, uint8_t level, const char *text)
{
    uint8_t b[CTP_OUT_BODY_MAX];
    const uint32_t n = ctp_enc_log(b, sizeof b, level, text);
    out_push(c, CTP_T_EVT_LOG, 0u, b, n);
}

/* ---- init ------------------------------------------------------------------------ */

void ctp_init(ctp_t *c, const ctp_port_t *port, const ctp_boot_info_t *boot)
{
    memset(c, 0, sizeof *c);
    ctp_crc32_init();
    c->port = port;
    c->boot = *boot;
    ctp_config_boot(&c->cfg);
    ctp_filter_empty(&c->filters[0]);
    ctp_filter_empty(&c->filters[1]);
    atomic_store(&c->active_filter, 0u);
    atomic_store(&c->cb_epoch, 0u);
    atomic_store(&c->session, false);
    atomic_store(&c->capture_on, false);
    atomic_store(&c->snaplen, 0u);
    atomic_store(&c->cfg_gen, 0u);
    atomic_store(&c->last_rx_ms, 0u);
    atomic_store(&c->flush_req, false);
    atomic_store(&c->rxseq_reset, true);
    atomic_store(&c->reboot_req, (uint32_t)CTP_RB_NONE);
    atomic_store(&c->reqq.head, 0u);
    atomic_store(&c->reqq.tail, 0u);
    atomic_store(&c->outq.head, 0u);
    atomic_store(&c->outq.tail, 0u);
    ctp_ring_init(&c->ring, c->ringbuf, CTP_RING_SIZE);
    ctp_frame_dec_init(&c->dec);
    c->first_tx = true;

    uint8_t b[CTP_OUT_BODY_MAX];
    const uint8_t sf = boot->safe_mode ? CTP_STATE_SAFE_MODE : 0u;
    const uint32_t n = ctp_enc_reset(b, sizeof b, (uint8_t)boot->reset_reason, boot->fw_version, sf, boot->reset_detail);
    out_push(c, CTP_T_EVT_RESET, 0u, b, n);
}

/* ---- request execution (ctrl task) -------------------------------------------------- */

typedef struct {
    uint8_t type;
    uint32_t len;
    uint32_t reboot; /* ctp_rb_req_t, applied after the response is queued */
    uint8_t body[CTP_OUT_BODY_MAX];
} rsp_t;

static void rsp_err(ctp_t *c, rsp_t *r, ctp_err_t e, const char *detail)
{
    int32_t esp = 0;
    if (e == CTP_E_RADIO || e == CTP_E_CHANNEL_NOT_ALLOWED || e == CTP_E_COUNTRY_INVALID) {
        esp = c->port->last_esp_err();
    }
    r->type = CTP_T_RSP_ERR;
    r->len = ctp_enc_rsp_err(r->body, sizeof r->body, (uint16_t)e, esp, detail);
}

static void rsp_config(ctp_t *c, rsp_t *r)
{
    r->type = CTP_T_RSP_OK;
    r->len = ctp_enc_config(r->body, sizeof r->body, &c->cfg);
}

static void rsp_cfg_or_err(ctp_t *c, rsp_t *r, ctp_err_t e)
{
    if (e == CTP_OK) {
        rsp_config(c, r);
    } else {
        rsp_err(c, r, e, NULL);
    }
}

static bool known_request(uint8_t t)
{
    switch (t) {
    case CTP_T_HELLO: case CTP_T_PING: case CTP_T_GET_CONFIG: case CTP_T_SET_CHANNEL: case CTP_T_SET_COUNTRY:
    case CTP_T_SET_FILTER: case CTP_T_SET_SNAPLEN: case CTP_T_SET_DRIVER_MASK: case CTP_T_CAPTURE:
    case CTP_T_GET_STATS: case CTP_T_REBOOT:
        return true;
    default:
        return false;
    }
}

static bool safe_mode_allowed(uint8_t t)
{
    return t == CTP_T_HELLO || t == CTP_T_PING || t == CTP_T_GET_CONFIG || t == CTP_T_GET_STATS || t == CTP_T_REBOOT;
}

static void exec(ctp_t *c, const ctp_req_slot_t *q, rsp_t *r)
{
    const uint8_t *b = q->body;
    const uint32_t len = q->len;
    if (c->boot.safe_mode && known_request(q->h.type) && !safe_mode_allowed(q->h.type)) {
        rsp_err(c, r, CTP_E_SAFE_MODE, "safe mode");
        return;
    }
    switch (q->h.type) {
    case CTP_T_HELLO: {
        ctp_hello_t m;
        if (ctp_dec_hello(b, len, &m) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        atomic_store_explicit(&c->session, true, memory_order_release);
        c->next_stats_us = c->port->now_us() + CTP_STATS_US;
        ctp_devinfo_t d;
        memset(&d, 0, sizeof d);
        d.proto_ver = CTP_PROTO_VER;
        d.fw_version = c->boot.fw_version;
        d.fw_flags = c->boot.fw_flags;
        memcpy(d.build_id, c->boot.build_id, sizeof d.build_id);
        memcpy(d.mac, c->boot.mac, sizeof d.mac);
        d.caps = c->boot.caps;
        d.max_frame = (uint16_t)CTP_MAX_FRAME;
        d.max_rules = (uint16_t)CTP_MAX_RULES;
        d.device_time_us = c->port->now_us();
        d.host_nonce = m.host_nonce;
        d.reset_reason = (uint8_t)c->boot.reset_reason;
        d.state_flags = c->boot.safe_mode ? CTP_STATE_SAFE_MODE : 0u;
        r->type = CTP_T_RSP_OK;
        r->len = ctp_enc_devinfo(r->body, sizeof r->body, &d);
        return;
    }
    case CTP_T_PING: {
        uint32_t t = 0u;
        if (ctp_dec_u32(b, len, &t) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        r->type = CTP_T_RSP_OK;
        r->len = ctp_enc_pong(r->body, sizeof r->body, t, c->port->now_us());
        return;
    }
    case CTP_T_GET_CONFIG:
        rsp_config(c, r);
        return;
    case CTP_T_SET_CHANNEL: {
        ctp_set_channel_t m;
        if (ctp_dec_set_channel(b, len, &m) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        rsp_cfg_or_err(c, r, cfg_set_channel(c, m.channel, m.bw));
        return;
    }
    case CTP_T_SET_COUNTRY: {
        ctp_country_t m;
        if (ctp_dec_country(b, len, &m) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        rsp_cfg_or_err(c, r, cfg_set_country(c, &m));
        return;
    }
    case CTP_T_SET_FILTER:
        rsp_cfg_or_err(c, r, cfg_set_filter(c, b, len, q->truncated));
        return;
    case CTP_T_SET_SNAPLEN: {
        uint16_t v = 0u;
        if (ctp_dec_u16(b, len, &v) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        ctp_config_t n = c->cfg;
        n.snaplen = v;
        cfg_commit(c, &n);
        rsp_config(c, r);
        return;
    }
    case CTP_T_SET_DRIVER_MASK: {
        ctp_driver_mask_t m;
        if (ctp_dec_driver_mask(b, len, &m) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        rsp_cfg_or_err(c, r, cfg_set_driver_mask(c, m.type_mask, m.ctrl_mask));
        return;
    }
    case CTP_T_CAPTURE: {
        uint8_t on = 0u;
        if (ctp_dec_u8(b, len, &on) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        if (on != 0u && !atomic_load_explicit(&c->session, memory_order_acquire)) {
            rsp_err(c, r, CTP_E_NO_SESSION, "HELLO first");
            return;
        }
        rsp_cfg_or_err(c, r, cfg_set_capture(c, on != 0u));
        return;
    }
    case CTP_T_GET_STATS: {
        uint8_t reset = 0u;
        if (ctp_dec_u8(b, len, &reset) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        ctp_stats_t st;
        ctp_stats_snapshot(c, &st, reset != 0u);
        r->type = CTP_T_RSP_OK;
        r->len = ctp_enc_stats(r->body, sizeof r->body, &st);
        return;
    }
    case CTP_T_REBOOT: {
        uint8_t mode = 0u;
        if (ctp_dec_u8(b, len, &mode) != CTP_OK) {
            rsp_err(c, r, CTP_E_BAD_BODY, NULL);
            return;
        }
        if (mode > 1u) {
            rsp_err(c, r, CTP_E_INVALID_ARG, "mode");
            return;
        }
        r->type = CTP_T_RSP_OK;
        r->len = 0u;
        r->reboot = (mode == 0u) ? (uint32_t)CTP_RB_NORMAL : (uint32_t)CTP_RB_DOWNLOAD;
        return;
    }
    default:
        rsp_err(c, r, CTP_E_UNKNOWN_TYPE, NULL);
        return;
    }
}

static void handle_request(ctp_t *c, const ctp_req_slot_t *q)
{
    uint32_t crc = ctp_crc32(q->body, q->len);
    if (q->truncated) {
        crc = ~crc;
    }
    for (uint32_t i = 0u; i < CTP_DEDUP_N; i++) {
        const ctp_dedup_t *d = &c->dedup[i];
        if (d->valid && d->req_id == q->h.req_id && d->type == q->h.type && d->crc == crc) {
            out_push(c, d->rtype, q->h.req_id, d->rbody, d->rlen);
            return;
        }
    }
    rsp_t r;
    r.type = CTP_T_RSP_ERR;
    r.len = 0u;
    r.reboot = (uint32_t)CTP_RB_NONE;
    exec(c, q, &r);
    ctp_dedup_t *d = &c->dedup[c->dedup_next];
    c->dedup_next = (c->dedup_next + 1u) % CTP_DEDUP_N;
    d->valid = true;
    d->req_id = q->h.req_id;
    d->type = q->h.type;
    d->crc = crc;
    d->rtype = r.type;
    d->rlen = (uint16_t)r.len;
    if (r.len > 0u) {
        memcpy(d->rbody, r.body, r.len);
    }
    out_push(c, r.type, q->h.req_id, r.body, r.len);
    if (r.reboot != (uint32_t)CTP_RB_NONE) {
        atomic_store_explicit(&c->reboot_req, r.reboot, memory_order_release);
    }
}

void ctp_ctrl_step(ctp_t *c, uint32_t timeout_ms)
{
    if (q_empty(&c->reqq)) {
        c->port->ctrl_wait(timeout_ms);
    }
    while (!q_empty(&c->reqq)) {
        handle_request(c, &c->req[q_tail(&c->reqq, CTP_REQ_SLOTS)]);
        q_pop(&c->reqq);
    }
    /* host watchdog (docs/03 §7.4) */
    if (atomic_load_explicit(&c->session, memory_order_acquire)) {
        const uint32_t last = atomic_load_explicit(&c->last_rx_ms, memory_order_acquire);
        if ((uint32_t)(now_ms(c) - last) > CTP_WATCHDOG_MS) {
            atomic_store_explicit(&c->session, false, memory_order_release);
            const ctp_err_t e = cfg_set_capture(c, false); /* promisc off; radio errors change nothing here */
            (void)e;
            atomic_store_explicit(&c->flush_req, true, memory_order_release);
        }
    }
    /* EVT_STATS every second while the host is alive */
    if (atomic_load_explicit(&c->session, memory_order_acquire)) {
        const uint64_t now = c->port->now_us();
        if (now >= c->next_stats_us) {
            c->next_stats_us += CTP_STATS_US;
            if (now >= c->next_stats_us) {
                c->next_stats_us = now + CTP_STATS_US;
            }
            ctp_stats_t st;
            ctp_stats_snapshot(c, &st, false);
            uint8_t b[CTP_OUT_BODY_MAX];
            const uint32_t n = ctp_enc_stats(b, sizeof b, &st);
            out_push(c, CTP_T_EVT_STATS, 0u, b, n);
        }
    }
}

/* ---- link task ---------------------------------------------------------------------- */

static bool on_frame(void *ctx, const ctp_hdr_t *h, const uint8_t *body, uint32_t len)
{
    ctp_t *c = (ctp_t *)ctx;
    if (h->ver != CTP_PROTO_VER) {
        atomic_fetch_add_explicit(&c->cnt.host_crc, 1u, memory_order_relaxed);
        return true;
    }
    atomic_store_explicit(&c->last_rx_ms, now_ms(c), memory_order_release);
    /* never called with a full queue: the link stops feeding first */
    CTP_ASSERT(!q_full(&c->reqq, CTP_REQ_SLOTS));
    ctp_req_slot_t *s = &c->req[q_head(&c->reqq, CTP_REQ_SLOTS)];
    s->h = *h;
    s->truncated = len > CTP_REQ_BODY_MAX;
    s->len = (uint16_t)(s->truncated ? CTP_REQ_BODY_MAX : len);
    if (s->len > 0u) {
        memcpy(s->body, body, s->len);
    }
    q_push(&c->reqq);
    c->port->ctrl_notify();
    return !q_full(&c->reqq, CTP_REQ_SLOTS); /* stop feeding: back-pressure to the host */
}

static bool tx_add(ctp_t *c, uint8_t type, uint16_t req_id, uint8_t gen, const uint8_t *body, uint32_t len)
{
    if (c->tx_len + ctp_frame_wire_max(len) > CTP_TX_BUF) {
        return false;
    }
    ctp_hdr_t h = {.ver = CTP_PROTO_VER, .type = type, .seq = c->tx_seq, .req_id = req_id, .flags = 0u, .cfg_gen = gen};
    if (!c->first_tx && gen != c->last_gen_tx) {
        h.flags = (uint8_t)(h.flags | CTP_HFLAG_CFG_CHANGED);
    }
    uint32_t n = 0u;
    const ctp_err_t e = ctp_frame_encode(&c->txbuf[c->tx_len], CTP_TX_BUF - c->tx_len, &h, body, len, &n);
    CTP_ASSERT(e == CTP_OK);
    c->tx_len += n;
    c->tx_seq = (uint16_t)(c->tx_seq + 1u);
    c->first_tx = false;
    c->last_gen_tx = gen;
    return true;
}

static void sync_decoder_stats(ctp_t *c)
{
    const ctp_dec_stats_t *s = &c->dec.st;
    const uint32_t crc = (s->crc_errors - c->dec_seen.crc_errors) + (s->cobs_errors - c->dec_seen.cobs_errors) +
                         (s->shortf - c->dec_seen.shortf);
    const uint32_t over = s->oversize - c->dec_seen.oversize;
    if (crc != 0u) {
        atomic_fetch_add_explicit(&c->cnt.host_crc, crc, memory_order_relaxed);
    }
    if (over != 0u) {
        atomic_fetch_add_explicit(&c->cnt.host_oversize, over, memory_order_relaxed);
    }
    c->dec_seen = *s;
}

void ctp_link_step(ctp_t *c)
{
    /* RX with back-pressure: leftover bytes stay in rxbuf (and further data in
     * the USB FIFO, which flow-controls the host) while the request queue is full. */
    if (!q_full(&c->reqq, CTP_REQ_SLOTS)) {
        if (c->rx_off >= c->rx_len) {
            /* block ≤1 ms only when idle; poll while TX work is pending, or
             * the read timeout would cap throughput at one batch per ms */
            uint32_t plen = 0u;
            const bool busy = !q_empty(&c->outq) || c->tx_off < c->tx_len ||
                              (atomic_load_explicit(&c->session, memory_order_acquire) &&
                               ctp_ring_peek(&c->ring, &plen) != NULL);
            const int32_t n = c->port->usb_read(c->rxbuf, CTP_RX_CHUNK, busy ? 0u : 1u);
            c->rx_len = (n > 0) ? (uint32_t)n : 0u;
            c->rx_off = 0u;
        }
        if (c->rx_off < c->rx_len) {
            c->rx_off += ctp_frame_dec_feed(&c->dec, &c->rxbuf[c->rx_off], c->rx_len - c->rx_off, on_frame, c);
            sync_decoder_stats(c);
        }
    } else {
        c->port->delay_ms(1u);
    }
    if (atomic_exchange_explicit(&c->flush_req, false, memory_order_acq_rel)) {
        ctp_ring_flush(&c->ring);
    }
    /* finish a partial write first; a host that stops reading must never
     * wedge the link (drop the batch after CTP_TX_STALL_MS, host sees a seq gap) */
    if (c->tx_off < c->tx_len) {
        const int32_t w = c->port->usb_write(&c->txbuf[c->tx_off], c->tx_len - c->tx_off, 5u);
        if (w > 0) {
            c->tx_off += (uint32_t)w;
            c->tx_progress_ms = now_ms(c);
        }
        if (c->tx_off < c->tx_len) {
            if ((uint32_t)(now_ms(c) - c->tx_progress_ms) <= CTP_TX_STALL_MS) {
                return;
            }
            /* stalled: fall through, the batch is discarded below */
        }
    }
    c->tx_len = 0u;
    c->tx_off = 0u;
    c->txbuf[c->tx_len++] = 0u; /* leading delimiter: cheap resync insurance */
    /* ctrl-out queue first (docs/03 §5 priority) */
    while (!q_empty(&c->outq)) {
        const ctp_out_slot_t *s = &c->out[q_tail(&c->outq, CTP_OUT_SLOTS)];
        if (!tx_add(c, s->type, s->req_id, s->cfg_gen, s->body, s->len)) {
            break;
        }
        q_pop(&c->outq);
    }
    const bool session = atomic_load_explicit(&c->session, memory_order_acquire);
    if (q_empty(&c->outq)) {
        uint32_t len = 0u;
        const uint8_t *rec;
        while (c->tx_len < CTP_TX_BATCH && (rec = ctp_ring_peek(&c->ring, &len)) != NULL) {
            if (!session) {
                ctp_ring_release(&c->ring); /* stale packets never leave without a session */
                continue;
            }
            const uint32_t before = c->tx_len;
            if (!tx_add(c, CTP_T_PKT, 0u, rec[0], &rec[1], len - 1u)) {
                break;
            }
            ctp_ring_release(&c->ring);
            atomic_fetch_add_explicit(&c->cnt.tx_frames, 1u, memory_order_relaxed);
            atomic_fetch_add_explicit(&c->cnt.tx_bytes, c->tx_len - before, memory_order_relaxed);
        }
    }
    if (c->tx_len > 1u) {
        c->tx_progress_ms = now_ms(c);
        const int32_t w = c->port->usb_write(c->txbuf, c->tx_len, 5u);
        c->tx_off = (w > 0) ? (uint32_t)w : 0u;
    } else {
        c->tx_len = 0u;
    }
    if (c->tx_off >= c->tx_len && q_empty(&c->outq)) {
        const uint32_t rb = atomic_exchange_explicit(&c->reboot_req, (uint32_t)CTP_RB_NONE, memory_order_acq_rel);
        if (rb != (uint32_t)CTP_RB_NONE) {
            c->port->reboot((rb == (uint32_t)CTP_RB_DOWNLOAD) ? CTP_REBOOT_DOWNLOAD : CTP_REBOOT_NORMAL);
        }
    }
}
