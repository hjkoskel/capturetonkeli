/* SET_FILTER decode/validate, compiled form and matcher (docs/04 App. A).
 * Must agree with ctp/filter.Match (Go reference) on every vector. */
#ifndef CTP_FILTER_H
#define CTP_FILTER_H

#include "ctp/common.h"

#define CTP_G_DROP_FCS   0x01u
#define CTP_G_RSSI_MIN   0x02u
#define CTP_G_LEN_MIN    0x04u
#define CTP_G_LEN_MAX    0x08u
#define CTP_T_NEGATE     0x01u

typedef struct {
    uint16_t offset;
    uint8_t len;
    bool negate;
    uint8_t value[CTP_MAX_TERM_LEN]; /* pre-masked: value & mask */
    uint8_t mask[CTP_MAX_TERM_LEN];
} ctp_term_t;

typedef struct {
    uint8_t n_terms;
    ctp_term_t terms[CTP_MAX_TERMS];
} ctp_rule_t;

typedef struct {
    uint8_t n_rules;
    uint8_t gflags;
    int8_t rssi_min;
    uint16_t len_min;
    uint16_t len_max;
    uint32_t crc32; /* CRC of the exact body */
    ctp_rule_t rules[CTP_MAX_RULES];
} ctp_filter_t;

/* Inputs to the matcher beyond the frame bytes. */
typedef struct {
    bool fcs_err;
    int8_t rssi;
    uint16_t orig_len;
} ctp_match_meta_t;

/* The empty filter (pass everything), crc = CTP_EMPTY_FILTER_CRC. */
void ctp_filter_empty(ctp_filter_t *f);

/* Strict decode: CTP_E_FILTER_INVALID on any violation (docs/03 §6.1).
 * Decodes in place; *out is undefined on failure. */
CTP_NODISCARD ctp_err_t ctp_filter_decode(const uint8_t *body, uint32_t len, uint8_t max_rules, ctp_filter_t *out);

bool ctp_filter_match(const ctp_filter_t *f, const uint8_t *frame, uint32_t len, const ctp_match_meta_t *m);

/* True if a rejection was caused by the drop_fcs_err condition (for drop_fcs). */
bool ctp_filter_fcs_drop(const ctp_filter_t *f, const ctp_match_meta_t *m);

#endif
