#include "ctp/wire.h"

#include <string.h>

#include "ctp/rd.h"

static ctp_err_t done(const ctp_rd_t *r)
{
    return r->err ? CTP_E_BAD_BODY : CTP_OK;
}

ctp_err_t ctp_dec_hello(const uint8_t *b, uint32_t len, ctp_hello_t *m)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    m->host_nonce = ctp_rd_u32(&r);
    m->host_proto_ver = ctp_rd_u8(&r);
    return done(&r);
}

ctp_err_t ctp_dec_u8(const uint8_t *b, uint32_t len, uint8_t *v)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    *v = ctp_rd_u8(&r);
    return done(&r);
}

ctp_err_t ctp_dec_u16(const uint8_t *b, uint32_t len, uint16_t *v)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    *v = ctp_rd_u16(&r);
    return done(&r);
}

ctp_err_t ctp_dec_u32(const uint8_t *b, uint32_t len, uint32_t *v)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    *v = ctp_rd_u32(&r);
    return done(&r);
}

ctp_err_t ctp_dec_set_channel(const uint8_t *b, uint32_t len, ctp_set_channel_t *m)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    m->channel = ctp_rd_u8(&r);
    m->bw = ctp_rd_u8(&r);
    return done(&r);
}

ctp_err_t ctp_dec_country(const uint8_t *b, uint32_t len, ctp_country_t *m)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    uint8_t cc[3];
    ctp_rd_bytes(&r, cc, 3u);
    m->cc[0] = (char)cc[0];
    m->cc[1] = (char)cc[1];
    m->cc[2] = (char)cc[2];
    m->mode = ctp_rd_u8(&r);
    m->schan = ctp_rd_u8(&r);
    m->nchan = ctp_rd_u8(&r);
    return done(&r);
}

ctp_err_t ctp_dec_driver_mask(const uint8_t *b, uint32_t len, ctp_driver_mask_t *m)
{
    ctp_rd_t r;
    ctp_rd_init(&r, b, len);
    m->type_mask = ctp_rd_u32(&r);
    m->ctrl_mask = ctp_rd_u32(&r);
    return done(&r);
}

static uint32_t fin(const ctp_wr_t *w)
{
    return w->err ? 0u : w->off;
}

uint32_t ctp_enc_devinfo(uint8_t *dst, uint32_t cap, const ctp_devinfo_t *m)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u8(&w, m->proto_ver);
    ctp_wr_u32(&w, m->fw_version);
    ctp_wr_u8(&w, m->fw_flags);
    ctp_wr_bytes(&w, m->build_id, 8u);
    ctp_wr_bytes(&w, m->mac, 6u);
    ctp_wr_u32(&w, m->caps);
    ctp_wr_u16(&w, m->max_frame);
    ctp_wr_u16(&w, m->max_rules);
    ctp_wr_u64(&w, m->device_time_us);
    ctp_wr_u32(&w, m->host_nonce);
    ctp_wr_u8(&w, m->reset_reason);
    ctp_wr_u8(&w, m->state_flags);
    return fin(&w);
}

uint32_t ctp_enc_pong(uint8_t *dst, uint32_t cap, uint32_t host_time_ms, uint64_t device_time_us)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u32(&w, host_time_ms);
    ctp_wr_u64(&w, device_time_us);
    return fin(&w);
}

uint32_t ctp_enc_config(uint8_t *dst, uint32_t cap, const ctp_config_t *m)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u8(&w, m->cfg_gen);
    ctp_wr_u8(&w, m->channel);
    ctp_wr_u8(&w, m->bw);
    ctp_wr_u8(&w, m->band);
    ctp_wr_u8(&w, (uint8_t)m->country.cc[0]);
    ctp_wr_u8(&w, (uint8_t)m->country.cc[1]);
    ctp_wr_u8(&w, (uint8_t)m->country.cc[2]);
    ctp_wr_u8(&w, m->country.mode);
    ctp_wr_u8(&w, m->country.schan);
    ctp_wr_u8(&w, m->country.nchan);
    ctp_wr_u16(&w, m->snaplen);
    ctp_wr_u32(&w, m->type_mask);
    ctp_wr_u32(&w, m->ctrl_mask);
    ctp_wr_u8(&w, m->capture_on);
    ctp_wr_u8(&w, m->n_rules);
    ctp_wr_u32(&w, m->rules_crc32);
    return fin(&w);
}

uint32_t ctp_enc_stats(uint8_t *dst, uint32_t cap, const ctp_stats_t *m)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u64(&w, m->device_time_us);
    ctp_wr_u32(&w, m->rx_total);
    ctp_wr_u32(&w, m->rx_passed);
    ctp_wr_u32(&w, m->drop_ring);
    ctp_wr_u32(&w, m->drop_fcs);
    ctp_wr_u32(&w, m->tx_frames);
    ctp_wr_u32(&w, m->tx_bytes);
    ctp_wr_u32(&w, m->host_rx_crc_err);
    ctp_wr_u32(&w, m->host_rx_oversize);
    ctp_wr_u16(&w, m->ring_high_water);
    ctp_wr_u8(&w, m->channel);
    ctp_wr_i8(&w, m->noise_floor);
    ctp_wr_u32(&w, m->flash_writes);
    return fin(&w);
}

static void wr_str8(ctp_wr_t *w, const char *s, uint32_t max)
{
    uint32_t n = 0u;
    if (s != NULL) {
        while (n < max && s[n] != '\0') {
            n++;
        }
    }
    ctp_wr_u8(w, (uint8_t)n);
    ctp_wr_bytes(w, (const uint8_t *)s, n);
}

uint32_t ctp_enc_rsp_err(uint8_t *dst, uint32_t cap, uint16_t code, int32_t esp_err, const char *detail)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u16(&w, code);
    ctp_wr_i32(&w, esp_err);
    wr_str8(&w, detail, CTP_MAX_DETAIL);
    return fin(&w);
}

uint32_t ctp_enc_reset(uint8_t *dst, uint32_t cap, uint8_t reason, uint32_t fw_version, uint8_t state_flags,
                       const char *detail)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u8(&w, reason);
    ctp_wr_u32(&w, fw_version);
    ctp_wr_u8(&w, state_flags);
    wr_str8(&w, detail, CTP_MAX_DETAIL);
    return fin(&w);
}

uint32_t ctp_enc_log(uint8_t *dst, uint32_t cap, uint8_t level, const char *text)
{
    ctp_wr_t w;
    ctp_wr_init(&w, dst, cap);
    ctp_wr_u8(&w, level);
    uint32_t n = 0u;
    while (text != NULL && n < 200u && text[n] != '\0') {
        n++;
    }
    ctp_wr_bytes(&w, (const uint8_t *)text, n);
    return fin(&w);
}

static void put16(uint8_t *p, uint16_t v)
{
    p[0] = (uint8_t)(v & 0xFFu);
    p[1] = (uint8_t)(v >> 8);
}

/* Hot path (promiscuous callback): fixed-offset stores instead of ~20 cursor
 * calls. Bounds are checked once up front. */
uint32_t ctp_enc_pkt_meta(uint8_t *dst, uint32_t cap, const ctp_pkt_meta_t *m)
{
    if (dst == NULL || cap < CTP_PKT_META_LEN) {
        return 0u;
    }
    for (uint32_t i = 0u; i < 8u; i++) {
        dst[i] = (uint8_t)((m->ts_us >> (8u * i)) & 0xFFu);
    }
    dst[8] = m->channel;
    dst[9] = m->flags;
    dst[10] = (uint8_t)m->rssi;
    dst[11] = (uint8_t)m->noise;
    dst[12] = m->rate;
    dst[13] = m->bb_format;
    put16(&dst[14], m->orig_len);
    put16(&dst[16], m->cap_len);
    put16(&dst[18], m->rx_seq);
    return CTP_PKT_META_LEN;
}

uint8_t ctp_rate_from_rxctrl(uint8_t bb_format, uint8_t raw_rate, bool *unknown)
{
    *unknown = false;
    if (bb_format == 0u) { /* 11b: driver rate code (long/short preamble) */
        switch (raw_rate) {
        case 0u: return 2u;
        case 1u: case 5u: return 4u;
        case 2u: case 6u: return 11u;
        case 3u: case 7u: return 22u;
        default: break;
        }
    } else if (bb_format == 1u) { /* 11g/a: L-SIG RATE field */
        switch (raw_rate) {
        case 0x0Bu: return 12u;
        case 0x0Fu: return 18u;
        case 0x0Au: return 24u;
        case 0x0Eu: return 36u;
        case 0x09u: return 48u;
        case 0x0Du: return 72u;
        case 0x08u: return 96u;
        case 0x0Cu: return 108u;
        default: break;
        }
    } else {
        /* HT/VHT/HE: MCS lives in the SIG fields; bit layout confirmed in M4. */
    }
    *unknown = true;
    return 0u;
}

uint8_t ctp_mcs_from_siga1(uint8_t bb_format, uint32_t siga1, bool *unknown)
{
    *unknown = false;
    switch (bb_format) {
    case 2u: /* HT-SIG1 bits 0..6 */
        return (uint8_t)(siga1 & 0x7Fu);
    case 4u: /* HE-SU */
    case 6u: /* HE-ER-SU: HE-SIG-A1 bits 3..6 */
        return (uint8_t)((siga1 >> 3) & 0x0Fu);
    default:
        *unknown = true;
        return 0u;
    }
}
