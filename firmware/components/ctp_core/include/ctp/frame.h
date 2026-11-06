/* Layer 1+2 framing: COBS(header|body|crc32) 0x00, resync, oversize (docs/03 §3–4). */
#ifndef CTP_FRAME_H
#define CTP_FRAME_H

#include "ctp/common.h"

typedef struct {
    uint8_t ver;
    uint8_t type;
    uint16_t seq;
    uint16_t req_id;
    uint8_t flags;
    uint8_t cfg_gen;
} ctp_hdr_t;

typedef struct {
    uint32_t frames;
    uint32_t crc_errors;
    uint32_t cobs_errors;
    uint32_t oversize;
    uint32_t shortf;
    uint32_t skipped;
} ctp_dec_stats_t;

/* Return false to stop feeding after this frame (receiver back-pressure). */
typedef bool (*ctp_frame_cb_t)(void *ctx, const ctp_hdr_t *h, const uint8_t *body, uint32_t len);

#define CTP_ENC_BUF (CTP_COBS_MAX(CTP_MAX_FRAME))

typedef struct {
    uint8_t enc[CTP_ENC_BUF];
    uint8_t dec[CTP_MAX_FRAME];
    uint32_t n;
    bool synced;
    bool overrun;
    ctp_dec_stats_t st;
} ctp_frame_dec_t;

void ctp_frame_dec_init(ctp_frame_dec_t *d);
/* Marks the stream as starting on a frame boundary. */
void ctp_frame_dec_set_synced(ctp_frame_dec_t *d);
/* Returns the number of bytes consumed (< len only if cb returned false). */
uint32_t ctp_frame_dec_feed(ctp_frame_dec_t *d, const uint8_t *p, uint32_t len, ctp_frame_cb_t cb, void *ctx);

/* Worst-case wire size of a frame with body_len bytes (incl. delimiter). */
uint32_t ctp_frame_wire_max(uint32_t body_len);

/* Appends COBS(header|body|crc) + 0x00 to dst; *out_len = bytes written. */
CTP_NODISCARD ctp_err_t ctp_frame_encode(uint8_t *dst, uint32_t cap, const ctp_hdr_t *h, const uint8_t *body,
                                         uint32_t body_len, uint32_t *out_len);

void ctp_hdr_put(uint8_t out[CTP_HDR_LEN], const ctp_hdr_t *h);

#endif
