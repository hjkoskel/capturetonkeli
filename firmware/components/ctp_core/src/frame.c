#include "ctp/frame.h"

#include "ctp/cobs.h"
#include "ctp/crc32.h"
#include "ctp/rd.h"

void ctp_frame_dec_init(ctp_frame_dec_t *d)
{
    d->n = 0u;
    d->synced = false;
    d->overrun = false;
    d->st = (ctp_dec_stats_t){0};
}

void ctp_frame_dec_set_synced(ctp_frame_dec_t *d)
{
    d->synced = true;
}

static bool handle(ctp_frame_dec_t *d, ctp_frame_cb_t cb, void *ctx)
{
    uint32_t n = 0u;
    const ctp_err_t e = ctp_cobs_decode(d->enc, d->n, d->dec, (uint32_t)sizeof d->dec, &n);
    if (e == CTP_E_OVERFLOW) {
        d->st.oversize++;
        return true;
    }
    if (e != CTP_OK) {
        d->st.cobs_errors++;
        return true;
    }
    if (n < CTP_HDR_LEN + CTP_CRC_LEN) {
        d->st.shortf++;
        return true;
    }
    const uint32_t m = n - CTP_CRC_LEN;
    ctp_rd_t r;
    ctp_rd_init(&r, &d->dec[m], CTP_CRC_LEN);
    if (ctp_crc32(d->dec, m) != ctp_rd_u32(&r)) {
        d->st.crc_errors++;
        return true;
    }
    ctp_rd_init(&r, d->dec, CTP_HDR_LEN);
    ctp_hdr_t h;
    h.ver = ctp_rd_u8(&r);
    h.type = ctp_rd_u8(&r);
    h.seq = ctp_rd_u16(&r);
    h.req_id = ctp_rd_u16(&r);
    h.flags = ctp_rd_u8(&r);
    h.cfg_gen = ctp_rd_u8(&r);
    d->st.frames++;
    if (cb != NULL) {
        return cb(ctx, &h, &d->dec[CTP_HDR_LEN], m - CTP_HDR_LEN);
    }
    return true;
}

uint32_t ctp_frame_dec_feed(ctp_frame_dec_t *d, const uint8_t *p, uint32_t len, ctp_frame_cb_t cb, void *ctx)
{
    for (uint32_t i = 0u; i < len; i++) {
        const uint8_t b = p[i];
        if (!d->synced) {
            if (b == 0u) {
                d->synced = true;
            } else {
                d->st.skipped++;
            }
            continue;
        }
        if (b != 0u) {
            if (d->overrun) {
                continue;
            }
            if (d->n >= (uint32_t)sizeof d->enc) {
                d->overrun = true;
                d->st.oversize++;
                continue;
            }
            d->enc[d->n] = b;
            d->n++;
            continue;
        }
        if (d->overrun) {
            d->overrun = false;
            d->n = 0u;
            continue;
        }
        if (d->n == 0u) {
            continue; /* empty frame ignored */
        }
        const bool more = handle(d, cb, ctx);
        d->n = 0u;
        if (!more) {
            return i + 1u;
        }
    }
    return len;
}

uint32_t ctp_frame_wire_max(uint32_t body_len)
{
    const uint32_t raw = CTP_HDR_LEN + body_len + CTP_CRC_LEN;
    return CTP_COBS_MAX(raw) + 1u;
}

void ctp_hdr_put(uint8_t out[CTP_HDR_LEN], const ctp_hdr_t *h)
{
    ctp_wr_t w;
    ctp_wr_init(&w, out, CTP_HDR_LEN);
    ctp_wr_u8(&w, h->ver);
    ctp_wr_u8(&w, h->type);
    ctp_wr_u16(&w, h->seq);
    ctp_wr_u16(&w, h->req_id);
    ctp_wr_u8(&w, h->flags);
    ctp_wr_u8(&w, h->cfg_gen);
}

ctp_err_t ctp_frame_encode(uint8_t *dst, uint32_t cap, const ctp_hdr_t *h, const uint8_t *body, uint32_t body_len,
                           uint32_t *out_len)
{
    uint8_t hdr[CTP_HDR_LEN];
    ctp_hdr_put(hdr, h);
    uint32_t crc = ctp_crc32_update(0u, hdr, CTP_HDR_LEN);
    crc = ctp_crc32_update(crc, body, body_len);
    uint8_t c[CTP_CRC_LEN];
    ctp_wr_t w;
    ctp_wr_init(&w, c, CTP_CRC_LEN);
    ctp_wr_u32(&w, crc);

    if (cap < 1u) {
        return CTP_E_OVERFLOW;
    }
    ctp_cobs_enc_t e;
    ctp_cobs_enc_begin(&e, dst, cap - 1u);
    ctp_cobs_enc_put(&e, hdr, CTP_HDR_LEN);
    ctp_cobs_enc_put(&e, body, body_len);
    ctp_cobs_enc_put(&e, c, CTP_CRC_LEN);
    uint32_t n = 0u;
    const ctp_err_t err = ctp_cobs_enc_end(&e, &n);
    if (err != CTP_OK) {
        return err;
    }
    dst[n] = 0u;
    *out_len = n + 1u;
    return CTP_OK;
}
