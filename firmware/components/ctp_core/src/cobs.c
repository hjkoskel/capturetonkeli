#include "ctp/cobs.h"

void ctp_cobs_enc_begin(ctp_cobs_enc_t *e, uint8_t *dst, uint32_t cap)
{
    e->dst = dst;
    e->cap = cap;
    e->off = 1u;
    e->code_idx = 0u;
    e->code = 1u;
    e->err = (cap < 1u);
}

static void put_byte(ctp_cobs_enc_t *e, uint8_t b)
{
    if (e->off >= e->cap) {
        e->err = true;
        return;
    }
    e->dst[e->off] = b;
    e->off++;
}

static void finish_block(ctp_cobs_enc_t *e)
{
    if (e->code_idx < e->cap) {
        e->dst[e->code_idx] = e->code;
    }
    e->code_idx = e->off;
    if (e->off >= e->cap) {
        e->err = true;
    } else {
        e->off++;
    }
    e->code = 1u;
}

void ctp_cobs_enc_put(ctp_cobs_enc_t *e, const uint8_t *src, uint32_t len)
{
    for (uint32_t i = 0u; i < len && !e->err; i++) {
        const uint8_t b = src[i];
        if (b == 0u) {
            finish_block(e);
            continue;
        }
        put_byte(e, b);
        e->code++;
        if (e->code == 0xFFu) {
            finish_block(e);
        }
    }
}

ctp_err_t ctp_cobs_enc_end(ctp_cobs_enc_t *e, uint32_t *out_len)
{
    if (e->err) {
        return CTP_E_OVERFLOW;
    }
    e->dst[e->code_idx] = e->code;
    *out_len = e->off;
    return CTP_OK;
}

ctp_err_t ctp_cobs_encode(const uint8_t *src, uint32_t len, uint8_t *dst, uint32_t cap, uint32_t *out_len)
{
    ctp_cobs_enc_t e;
    ctp_cobs_enc_begin(&e, dst, cap);
    ctp_cobs_enc_put(&e, src, len);
    return ctp_cobs_enc_end(&e, out_len);
}

ctp_err_t ctp_cobs_decode(const uint8_t *src, uint32_t len, uint8_t *dst, uint32_t cap, uint32_t *out_len)
{
    uint32_t i = 0u;
    uint32_t o = 0u;
    while (i < len) {
        const uint8_t code = src[i];
        if (code == 0u) {
            return CTP_E_COBS;
        }
        i++;
        const uint32_t n = (uint32_t)code - 1u;
        if (n > len - i) {
            return CTP_E_COBS;
        }
        if (n > cap - o) {
            return CTP_E_OVERFLOW;
        }
        for (uint32_t k = 0u; k < n; k++) {
            const uint8_t b = src[i + k];
            if (b == 0u) {
                return CTP_E_COBS;
            }
            dst[o + k] = b;
        }
        o += n;
        i += n;
        if (code != 0xFFu && i < len) {
            if (o >= cap) {
                return CTP_E_OVERFLOW;
            }
            dst[o] = 0u;
            o++;
        }
    }
    *out_len = o;
    return CTP_OK;
}
