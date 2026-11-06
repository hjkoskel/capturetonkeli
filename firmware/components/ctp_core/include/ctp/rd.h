/*
 * Bounds-checked little-endian cursors with a sticky error flag
 * (docs/07 §4 rule 3). Reading past the end yields zeros and sets err;
 * writing past the end writes nothing and sets err. Check err once at the end.
 */
#ifndef CTP_RD_H
#define CTP_RD_H

#include "ctp/common.h"

typedef struct {
    const uint8_t *p;
    uint32_t len;
    uint32_t off;
    bool err;
} ctp_rd_t;

typedef struct {
    uint8_t *p;
    uint32_t cap;
    uint32_t off;
    bool err;
} ctp_wr_t;

void ctp_rd_init(ctp_rd_t *r, const uint8_t *p, uint32_t len);
uint8_t ctp_rd_u8(ctp_rd_t *r);
int8_t ctp_rd_i8(ctp_rd_t *r);
uint16_t ctp_rd_u16(ctp_rd_t *r);
uint32_t ctp_rd_u32(ctp_rd_t *r);
int32_t ctp_rd_i32(ctp_rd_t *r);
uint64_t ctp_rd_u64(ctp_rd_t *r);
/* Copies n bytes into dst (zeros on error). */
void ctp_rd_bytes(ctp_rd_t *r, uint8_t *dst, uint32_t n);
/* Returns a pointer to n bytes in place, or NULL (and sets err). */
const uint8_t *ctp_rd_ptr(ctp_rd_t *r, uint32_t n);
uint32_t ctp_rd_left(const ctp_rd_t *r);

void ctp_wr_init(ctp_wr_t *w, uint8_t *p, uint32_t cap);
void ctp_wr_u8(ctp_wr_t *w, uint8_t v);
void ctp_wr_i8(ctp_wr_t *w, int8_t v);
void ctp_wr_u16(ctp_wr_t *w, uint16_t v);
void ctp_wr_u32(ctp_wr_t *w, uint32_t v);
void ctp_wr_i32(ctp_wr_t *w, int32_t v);
void ctp_wr_u64(ctp_wr_t *w, uint64_t v);
void ctp_wr_bytes(ctp_wr_t *w, const uint8_t *src, uint32_t n);

#endif
