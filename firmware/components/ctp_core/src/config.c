/* Config model, validation and apply sequencing through the port (ctrl task only). */
#include "config.h"

#include <string.h>

void ctp_config_boot(ctp_config_t *cfg)
{
    memset(cfg, 0, sizeof *cfg);
    cfg->channel = 1u;
    cfg->bw = (uint8_t)CTP_BW20;
    cfg->band = 0u;
    cfg->country.cc[0] = '0';
    cfg->country.cc[1] = '1';
    cfg->country.cc[2] = '\0';
    cfg->country.mode = 0u;
    cfg->country.schan = 1u;
    cfg->country.nchan = 11u;
    cfg->snaplen = 0u;
    cfg->type_mask = CTP_DEFAULT_TYPE_MASK;
    cfg->ctrl_mask = CTP_DEFAULT_CTRL_MASK;
    cfg->capture_on = 0u;
    cfg->n_rules = 0u;
    cfg->rules_crc32 = CTP_EMPTY_FILTER_CRC;
}

bool ctp_config_same(const ctp_config_t *a, const ctp_config_t *b)
{
    return a->channel == b->channel && a->bw == b->bw && a->band == b->band && a->country.cc[0] == b->country.cc[0] &&
           a->country.cc[1] == b->country.cc[1] && a->country.cc[2] == b->country.cc[2] &&
           a->country.mode == b->country.mode && a->country.schan == b->country.schan &&
           a->country.nchan == b->country.nchan && a->snaplen == b->snaplen && a->type_mask == b->type_mask &&
           a->ctrl_mask == b->ctrl_mask && a->capture_on == b->capture_on && a->n_rules == b->n_rules &&
           a->rules_crc32 == b->rules_crc32;
}

void cfg_commit(ctp_t *c, const ctp_config_t *next)
{
    const bool was_on = c->cfg.capture_on != 0u;
    if (ctp_config_same(&c->cfg, next)) {
        return;
    }
    const uint8_t gen = (uint8_t)(c->cfg.cfg_gen + 1u);
    c->cfg = *next;
    c->cfg.cfg_gen = gen;
    atomic_store_explicit(&c->snaplen, c->cfg.snaplen, memory_order_relaxed);
    atomic_store_explicit(&c->cfg_gen, gen, memory_order_release);
    atomic_store_explicit(&c->capture_on, c->cfg.capture_on != 0u, memory_order_release);
    if (was_on && c->cfg.capture_on == 0u) {
        atomic_store_explicit(&c->flush_req, true, memory_order_release);
    }
}

static bool channel_exists(uint8_t ch)
{
    return (ch >= 1u && ch <= 14u) || (ch >= 32u && ch <= 177u);
}

ctp_err_t cfg_set_channel(ctp_t *c, uint8_t ch, uint8_t bw)
{
    if (bw > (uint8_t)CTP_BW40_MINUS) {
        return CTP_E_INVALID_ARG;
    }
    if (!channel_exists(ch)) {
        return CTP_E_CHANNEL_NOT_ALLOWED;
    }
    const bool five = ch > 14u;
    if (five && (c->boot.caps & CTP_CAP_5GHZ) == 0u) {
        return CTP_E_NOT_SUPPORTED;
    }
    if (bw != (uint8_t)CTP_BW20 && (c->boot.caps & CTP_CAP_HT40) == 0u) {
        return CTP_E_NOT_SUPPORTED;
    }
    ctp_bw_t applied = CTP_BW20;
    const ctp_err_t e = c->port->radio_set_channel(ch, (ctp_bw_t)bw, &applied);
    if (e != CTP_OK) {
        return e;
    }
    ctp_config_t n = c->cfg;
    n.channel = ch;
    n.bw = (uint8_t)applied;
    n.band = five ? 1u : 0u;
    cfg_commit(c, &n);
    return CTP_OK;
}

static bool printable2(const char cc[3])
{
    for (uint32_t i = 0u; i < 2u; i++) {
        const uint8_t u = (uint8_t)cc[i];
        if (u < 0x21u || u > 0x7Eu) {
            return false;
        }
    }
    return true;
}

ctp_err_t cfg_set_country(ctp_t *c, const ctp_country_t *req)
{
    if (req->mode > 1u) {
        return CTP_E_INVALID_ARG;
    }
    ctp_country_t cc = *req;
    cc.cc[2] = '\0';
    if (cc.mode == 1u) {
        if ((c->boot.caps & CTP_CAP_MANUAL_COUNTRY) == 0u) {
            return CTP_E_NOT_SUPPORTED;
        }
        if (!printable2(cc.cc) || cc.schan < 1u || cc.nchan < 1u || ((uint32_t)cc.schan + cc.nchan - 1u) > 14u) {
            return CTP_E_COUNTRY_INVALID;
        }
    }
    const ctp_err_t e = c->port->radio_set_country(&cc);
    if (e != CTP_OK) {
        return e;
    }
    /* Re-apply the active channel under the new table (docs/03 §6.1). */
    ctp_bw_t applied = CTP_BW20;
    uint8_t ch = c->cfg.channel;
    if (c->port->radio_set_channel(ch, (ctp_bw_t)c->cfg.bw, &applied) != CTP_OK) {
        if (c->port->radio_set_channel(ch, CTP_BW20, &applied) != CTP_OK) {
            ch = cc.schan;
            if (c->port->radio_set_channel(ch, CTP_BW20, &applied) != CTP_OK) {
                return CTP_E_RADIO;
            }
        }
    }
    ctp_config_t n = c->cfg;
    n.country = cc;
    n.channel = ch;
    n.bw = (uint8_t)applied;
    n.band = (ch > 14u) ? 1u : 0u;
    cfg_commit(c, &n);
    return CTP_OK;
}

ctp_err_t cfg_set_driver_mask(ctp_t *c, uint32_t type_mask, uint32_t ctrl_mask)
{
    const ctp_err_t e = c->port->radio_set_driver_mask(type_mask, ctrl_mask);
    if (e != CTP_OK) {
        return e;
    }
    ctp_config_t n = c->cfg;
    n.type_mask = type_mask;
    n.ctrl_mask = ctrl_mask;
    cfg_commit(c, &n);
    return CTP_OK;
}

ctp_err_t cfg_set_capture(ctp_t *c, bool on)
{
    const bool cur = c->cfg.capture_on != 0u;
    if (on && !cur) {
        atomic_store_explicit(&c->rxseq_reset, true, memory_order_release);
    }
    if (on != cur) {
        const ctp_err_t e = c->port->radio_promisc(on);
        if (e != CTP_OK) {
            return e;
        }
    }
    ctp_config_t n = c->cfg;
    n.capture_on = on ? 1u : 0u;
    cfg_commit(c, &n);
    return CTP_OK;
}

/* Wait until no callback still uses the slot we just retired (docs/07 §3). */
static void wait_callback_epoch(ctp_t *c)
{
    const uint32_t e0 = atomic_load_explicit(&c->cb_epoch, memory_order_acquire);
    if ((e0 & 1u) == 0u) {
        return;
    }
    for (uint32_t i = 0u; i < 100u; i++) {
        if (atomic_load_explicit(&c->cb_epoch, memory_order_acquire) != e0) {
            return;
        }
        c->port->delay_ms(1u);
    }
    CTP_ASSERT(false); /* callback stuck > 100 ms */
}

ctp_err_t cfg_set_filter(ctp_t *c, const uint8_t *body, uint32_t len, bool truncated)
{
    if (truncated) {
        return CTP_E_FILTER_INVALID;
    }
    const uint32_t act = atomic_load_explicit(&c->active_filter, memory_order_relaxed);
    const uint32_t nxt = act ^ 1u;
    ctp_filter_t *slot = &c->filters[nxt];
    if (ctp_filter_decode(body, len, CTP_MAX_RULES, slot) != CTP_OK) {
        return CTP_E_FILTER_INVALID;
    }
    atomic_store_explicit(&c->active_filter, nxt, memory_order_release);
    wait_callback_epoch(c);
    ctp_config_t n = c->cfg;
    n.n_rules = slot->n_rules;
    n.rules_crc32 = slot->crc32;
    cfg_commit(c, &n);
    return CTP_OK;
}
