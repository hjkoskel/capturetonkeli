#include "ctp/filter.h"

#include <string.h>

#include "ctp/crc32.h"
#include "ctp/rd.h"

CTP_STATIC_ASSERT(sizeof(ctp_filter_t) < 2600u, "compiled filter slot stays ~2.4 KB (docs/07 §3)");

void ctp_filter_empty(ctp_filter_t *f)
{
    memset(f, 0, sizeof *f);
    f->crc32 = CTP_EMPTY_FILTER_CRC;
}

ctp_err_t ctp_filter_decode(const uint8_t *body, uint32_t len, uint8_t max_rules, ctp_filter_t *out)
{
    /* Decodes in place: on failure *out is garbage, so callers decode into
     * the inactive slot only (no 2.4 KB copy on the ctrl task stack). */
    ctp_rd_t r;
    ctp_rd_init(&r, body, len);
    ctp_filter_t *const fp = out;
    memset(fp, 0, sizeof *fp);
    fp->n_rules = ctp_rd_u8(&r);
    fp->gflags = ctp_rd_u8(&r);
    fp->rssi_min = ctp_rd_i8(&r);
    const uint8_t reserved = ctp_rd_u8(&r);
    fp->len_min = ctp_rd_u16(&r);
    fp->len_max = ctp_rd_u16(&r);
    if (r.err || reserved != 0u || (fp->gflags & (uint8_t)~0x0Fu) != 0u || fp->n_rules > CTP_MAX_RULES ||
        fp->n_rules > max_rules) {
        return CTP_E_FILTER_INVALID;
    }
    for (uint32_t i = 0u; i < fp->n_rules; i++) {
        ctp_rule_t *ru = &fp->rules[i];
        ru->n_terms = ctp_rd_u8(&r);
        if (r.err || ru->n_terms < 1u || ru->n_terms > CTP_MAX_TERMS) {
            return CTP_E_FILTER_INVALID;
        }
        for (uint32_t k = 0u; k < ru->n_terms; k++) {
            ctp_term_t *t = &ru->terms[k];
            t->offset = ctp_rd_u16(&r);
            t->len = ctp_rd_u8(&r);
            const uint8_t tf = ctp_rd_u8(&r);
            if (r.err || t->len < 1u || t->len > CTP_MAX_TERM_LEN || (tf & (uint8_t)~CTP_T_NEGATE) != 0u) {
                return CTP_E_FILTER_INVALID;
            }
            t->negate = (tf & CTP_T_NEGATE) != 0u;
            ctp_rd_bytes(&r, t->value, t->len);
            ctp_rd_bytes(&r, t->mask, t->len);
            if (r.err) {
                return CTP_E_FILTER_INVALID;
            }
            for (uint32_t j = 0u; j < t->len; j++) {
                t->value[j] = (uint8_t)(t->value[j] & t->mask[j]);
            }
        }
    }
    if (ctp_rd_left(&r) != 0u) {
        return CTP_E_FILTER_INVALID;
    }
    fp->crc32 = ctp_crc32(body, len);
    return CTP_OK;
}

static bool globals_pass(const ctp_filter_t *f, const ctp_match_meta_t *m)
{
    if ((f->gflags & CTP_G_DROP_FCS) != 0u && m->fcs_err) {
        return false;
    }
    if ((f->gflags & CTP_G_RSSI_MIN) != 0u && m->rssi < f->rssi_min) {
        return false;
    }
    if ((f->gflags & CTP_G_LEN_MIN) != 0u && m->orig_len < f->len_min) {
        return false;
    }
    if ((f->gflags & CTP_G_LEN_MAX) != 0u && m->orig_len > f->len_max) {
        return false;
    }
    return true;
}

bool ctp_filter_fcs_drop(const ctp_filter_t *f, const ctp_match_meta_t *m)
{
    return (f->gflags & CTP_G_DROP_FCS) != 0u && m->fcs_err;
}

static bool term_match(const ctp_term_t *t, const uint8_t *frame, uint32_t len)
{
    bool eq = ((uint32_t)t->offset + t->len) <= len;
    if (eq) {
        const uint8_t *p = &frame[t->offset];
        for (uint32_t i = 0u; i < t->len; i++) {
            if ((uint8_t)(p[i] & t->mask[i]) != t->value[i]) {
                eq = false;
                break;
            }
        }
    }
    return eq != t->negate;
}

bool ctp_filter_match(const ctp_filter_t *f, const uint8_t *frame, uint32_t len, const ctp_match_meta_t *m)
{
    if (!globals_pass(f, m)) {
        return false;
    }
    if (f->n_rules == 0u) {
        return true;
    }
    for (uint32_t i = 0u; i < f->n_rules && i < CTP_MAX_RULES; i++) {
        const ctp_rule_t *ru = &f->rules[i];
        bool all = true;
        for (uint32_t k = 0u; k < ru->n_terms && k < CTP_MAX_TERMS; k++) {
            if (!term_match(&ru->terms[k], frame, len)) {
                all = false;
                break;
            }
        }
        if (all) {
            return true;
        }
    }
    return false;
}
