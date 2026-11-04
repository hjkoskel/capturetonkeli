/* Fake radio for ctp_fw_host: synthetic 802.11 traffic, ESP-IDF-like country
 * table (the same model as ctp/regdom, via generated tv_countries.h), HT40
 * partner rules and driver type masks. */
#include <stdatomic.h>
#include <string.h>

#include "ctp/crc32.h"
#include "port_host.h"
#include "tv_countries.h"

typedef struct {
    char cc[3];
    uint8_t schan, nchan;
    const uint8_t *ch5;
    size_t n5;
} region_t;

static ctp_t *s_core;
static uint32_t s_rng;
static uint32_t s_fps;
static region_t s_region;
/* written by the ctrl thread (port calls), read by the radio thread */
static _Atomic uint8_t s_channel;
static _Atomic uint32_t s_bw;
static _Atomic uint32_t s_type_mask, s_ctrl_mask;
static _Atomic bool s_promisc;
static int32_t s_last_err;
static uint64_t s_last_tick_us;
static uint64_t s_carry_milli; /* frame fraction ×1000 */
static uint16_t s_seqno;

#define ESP_ERR_INVALID_ARG  0x102
#define ESP_ERR_WIFI_ARG     0x300A

static uint32_t rnd(void)
{
    /* xorshift32 */
    uint32_t x = s_rng;
    x ^= x << 13;
    x ^= x >> 17;
    x ^= x << 5;
    s_rng = x;
    return x;
}

static const tv_country_t *lookup(const char cc[2])
{
    for (size_t i = 0; i < TV_COUNTRIES_N; i++) {
        if (tv_countries[i].cc[0] == cc[0] && tv_countries[i].cc[1] == cc[1]) {
            return &tv_countries[i];
        }
    }
    return NULL;
}

static void set_region(const tv_country_t *t, const char cc[2], uint8_t schan, uint8_t nchan)
{
    s_region.cc[0] = cc[0];
    s_region.cc[1] = cc[1];
    s_region.cc[2] = '\0';
    s_region.schan = schan;
    s_region.nchan = nchan;
    s_region.ch5 = t->ch5;
    s_region.n5 = t->n5;
}

void radio_fake_reset(void)
{
    const tv_country_t *w = lookup("01");
    set_region(w, "01", w->schan, w->nchan);
    s_channel = 1u;
    s_bw = (uint32_t)CTP_BW20;
    s_type_mask = CTP_DEFAULT_TYPE_MASK;
    s_ctrl_mask = CTP_DEFAULT_CTRL_MASK;
    s_promisc = false;
    s_last_err = 0;
    s_last_tick_us = 0u;
    s_carry_milli = 0u;
}

void radio_fake_init(ctp_t *core, uint32_t seed, uint32_t frames_per_s)
{
    s_core = core;
    s_rng = (seed == 0u) ? 0x12345678u : seed;
    s_fps = frames_per_s;
    radio_fake_reset();
}

int32_t radio_fake_last_err(void)
{
    return s_last_err;
}

static bool allowed(uint8_t ch)
{
    if (ch >= 1u && ch <= 14u) {
        return ch >= s_region.schan && (uint32_t)ch < (uint32_t)s_region.schan + s_region.nchan;
    }
    for (size_t i = 0; i < s_region.n5; i++) {
        if (s_region.ch5[i] == ch) {
            return true;
        }
    }
    return false;
}

/* Mirror of regdom.HT40Partner. */
static bool ht40_partner(uint8_t ch, bool want_plus, uint8_t *partner, bool *plus)
{
    if (ch >= 1u && ch <= 14u) {
        if (want_plus) {
            if (ch + 4u > 13u) {
                return false;
            }
            *partner = (uint8_t)(ch + 4u);
            *plus = true;
            return true;
        }
        if (ch < 5u) {
            return false;
        }
        *partner = (uint8_t)(ch - 4u);
        *plus = false;
        return true;
    }
    uint32_t idx;
    if (ch >= 149u) {
        if (((uint32_t)ch - 149u) % 4u != 0u) {
            return false;
        }
        idx = ((uint32_t)ch - 149u) / 4u;
    } else {
        if (ch < 36u || (ch % 4u) != 0u) {
            return false;
        }
        idx = ((uint32_t)ch - 36u) / 4u;
    }
    if ((idx % 2u) == 0u) {
        *partner = (uint8_t)(ch + 4u);
        *plus = true;
    } else {
        *partner = (uint8_t)(ch - 4u);
        *plus = false;
    }
    return true;
}

ctp_err_t radio_fake_set_country(ctp_country_t *c)
{
    const tv_country_t *t = lookup(c->cc);
    if (c->mode == 0u) {
        if (t == NULL) {
            s_last_err = ESP_ERR_INVALID_ARG;
            return CTP_E_COUNTRY_INVALID;
        }
        set_region(t, c->cc, t->schan, t->nchan);
        c->schan = t->schan;
        c->nchan = t->nchan;
    } else {
        if (t == NULL) {
            t = lookup("01");
        }
        set_region(t, c->cc, c->schan, c->nchan);
    }
    s_last_err = 0;
    return CTP_OK;
}

ctp_err_t radio_fake_set_channel(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied)
{
    if (!allowed(ch)) {
        s_last_err = ESP_ERR_WIFI_ARG;
        return CTP_E_CHANNEL_NOT_ALLOWED;
    }
    ctp_bw_t a = CTP_BW20;
    if (bw != CTP_BW20) {
        uint8_t p = 0u;
        bool plus = false;
        if (!ht40_partner(ch, bw == CTP_BW40_PLUS, &p, &plus) || !allowed(p)) {
            s_last_err = ESP_ERR_WIFI_ARG;
            return CTP_E_CHANNEL_NOT_ALLOWED;
        }
        a = plus ? CTP_BW40_PLUS : CTP_BW40_MINUS;
    }
    s_channel = ch;
    s_bw = (uint32_t)a;
    *applied = a;
    s_last_err = 0;
    return CTP_OK;
}

ctp_err_t radio_fake_set_driver_mask(uint32_t type_mask, uint32_t ctrl_mask)
{
    s_type_mask = type_mask;
    s_ctrl_mask = ctrl_mask;
    return CTP_OK;
}

ctp_err_t radio_fake_promisc(bool on)
{
    s_promisc = on;
    return CTP_OK;
}

/* ---- traffic -------------------------------------------------------------------- */

static bool driver_pass(const uint8_t *f, uint32_t len)
{
    if (len < 1u) {
        return false;
    }
    const uint32_t t = ((uint32_t)f[0] >> 2) & 3u;
    const uint32_t sub = (uint32_t)f[0] >> 4;
    switch (t) {
    case 0u:
        return (s_type_mask & 1u) != 0u;
    case 1u:
        if ((s_type_mask & 2u) == 0u) {
            return false;
        }
        return (sub < 7u) || ((s_ctrl_mask & (1u << (16u + sub))) != 0u);
    case 2u:
        return (s_type_mask & 4u) != 0u;
    default:
        return (s_type_mask & 8u) != 0u;
    }
}

static uint32_t put(uint8_t *f, uint32_t n, const uint8_t *src, uint32_t len)
{
    memcpy(&f[n], src, len);
    return n + len;
}

static uint32_t mac_hdr(uint8_t *f, uint8_t fc0, uint8_t fc1, const uint8_t a1[6], const uint8_t a2[6],
                        const uint8_t a3[6])
{
    uint32_t n = 0u;
    f[n++] = fc0;
    f[n++] = fc1;
    f[n++] = 0u;
    f[n++] = 0u;
    n = put(f, n, a1, 6u);
    n = put(f, n, a2, 6u);
    n = put(f, n, a3, 6u);
    s_seqno = (uint16_t)(s_seqno + 1u);
    f[n++] = (uint8_t)(((uint32_t)s_seqno << 4) & 0xF0u);
    f[n++] = (uint8_t)(s_seqno >> 4);
    return n;
}

static void emit_one(uint64_t now)
{
    static uint8_t f[1600];
    static const uint8_t bc[6] = {0xff, 0xff, 0xff, 0xff, 0xff, 0xff};
    const uint8_t ch = s_channel;
    uint8_t ap[6] = {0x02, 0xc5, 0xaa, 0x00, ch, (uint8_t)(rnd() % 3u)};
    uint8_t sta[6] = {0x02, 0x11, 0x22, 0x33, 0x44, (uint8_t)(rnd() % 8u)};
    uint32_t n = 0u;
    uint8_t bb = (uint8_t)((ch <= 14u && (rnd() % 4u) == 0u) ? 0u : 1u);
    uint8_t raw_rate = (uint8_t)((bb == 0u) ? 0u : 0x0Bu);
    const uint32_t r = rnd() % 100u;
    if (r < 12u) { /* beacon */
        n = mac_hdr(f, 0x80u, 0u, bc, ap, ap);
        memset(&f[n], 0, 8u);
        n += 8u;
        const uint8_t fixed[] = {0x64, 0x00, 0x11, 0x04, 0x00, 0x07, 'f', 'w', 'h', 'o', 's', 't', 'a'};
        n = put(f, n, fixed, sizeof fixed);
        const uint8_t ds[] = {0x03, 0x01, ch};
        n = put(f, n, ds, sizeof ds);
    } else if (r < 18u) { /* probe request */
        n = mac_hdr(f, 0x40u, 0u, bc, sta, bc);
        const uint8_t ies[] = {0x00, 0x00, 0x01, 0x04, 0x8c, 0x12, 0x98, 0x24};
        n = put(f, n, ies, sizeof ies);
    } else if (r < 36u) { /* ACK */
        f[n++] = 0xd4u;
        f[n++] = 0u;
        f[n++] = 0u;
        f[n++] = 0u;
        n = put(f, n, sta, 6u);
    } else if (r < 42u) { /* RTS */
        f[n++] = 0xb4u;
        f[n++] = 0u;
        f[n++] = 0x20u;
        f[n++] = 0u;
        n = put(f, n, ap, 6u);
        n = put(f, n, sta, 6u);
    } else { /* QoS data, random payload */
        n = mac_hdr(f, 0x88u, 0x02u, sta, ap, ap);
        f[n++] = 0u;
        f[n++] = 0u;
        const uint8_t llc[] = {0xaa, 0xaa, 0x03, 0x00, 0x00, 0x00, 0x08, 0x00};
        n = put(f, n, llc, sizeof llc);
        const uint32_t pl = 20u + rnd() % 1400u;
        for (uint32_t i = 0u; i < pl; i++) {
            f[n + i] = (uint8_t)rnd();
        }
        n += pl;
        if ((rnd() % 3u) != 0u) {
            bb = 2u;
            raw_rate = (uint8_t)(rnd() % 8u);
        }
    }
    const uint32_t fcs = ctp_crc32(f, n);
    f[n++] = (uint8_t)(fcs & 0xFFu);
    f[n++] = (uint8_t)((fcs >> 8) & 0xFFu);
    f[n++] = (uint8_t)((fcs >> 16) & 0xFFu);
    f[n++] = (uint8_t)(fcs >> 24);
    const bool bad = (rnd() % 100u) == 0u;
    if (bad) {
        f[n - 1u] ^= 0x5Au;
    }
    if (!driver_pass(f, n)) {
        return;
    }
    ctp_rx_t rx;
    memset(&rx, 0, sizeof rx);
    rx.data = f;
    rx.len = (uint16_t)n;
    rx.rssi = (int8_t)(-40 - (int32_t)(rnd() % 45u));
    rx.noise = -95;
    rx.bb_format = bb;
    rx.rate = ctp_rate_from_rxctrl(bb, raw_rate, &rx.rate_unknown);
    rx.channel = ch;
    rx.ht40 = s_bw != (uint32_t)CTP_BW20;
    rx.fcs_err = bad;
    rx.ts_us = now;
    ctp_capture_rx(s_core, &rx);
}

void radio_fake_tick(void)
{
    const uint64_t now = sys_host_now_us();
    if (s_last_tick_us == 0u) {
        s_last_tick_us = now;
        return;
    }
    uint64_t dt = now - s_last_tick_us;
    s_last_tick_us = now;
    if (!s_promisc) {
        return;
    }
    if (dt > 100000u) {
        dt = 100000u;
    }
    s_carry_milli += dt * s_fps / 1000u; /* frames × 1000 */
    while (s_carry_milli >= 1000u) {
        s_carry_milli -= 1000u;
        emit_one(now);
    }
}
