/* COBS (Cheshire & Baker) encode/decode, byte-identical to ctp/codec (Go). */
#ifndef CTP_COBS_H
#define CTP_COBS_H

#include "ctp/common.h"

/* Streaming encoder: begin → put… → end. Writes no delimiter. */
typedef struct {
    uint8_t *dst;
    uint32_t cap;
    uint32_t off;
    uint32_t code_idx;
    uint8_t code;
    bool err;
} ctp_cobs_enc_t;

void ctp_cobs_enc_begin(ctp_cobs_enc_t *e, uint8_t *dst, uint32_t cap);
void ctp_cobs_enc_put(ctp_cobs_enc_t *e, const uint8_t *src, uint32_t len);
/* Returns encoded length, or CTP_E_OVERFLOW if dst was too small. */
CTP_NODISCARD ctp_err_t ctp_cobs_enc_end(ctp_cobs_enc_t *e, uint32_t *out_len);

/* One-shot encode. */
CTP_NODISCARD ctp_err_t ctp_cobs_encode(const uint8_t *src, uint32_t len, uint8_t *dst, uint32_t cap, uint32_t *out_len);

/* Decode one frame (without delimiter). CTP_E_COBS on invalid input,
 * CTP_E_OVERFLOW if the result does not fit. */
CTP_NODISCARD ctp_err_t ctp_cobs_decode(const uint8_t *src, uint32_t len, uint8_t *dst, uint32_t cap, uint32_t *out_len);

#endif
