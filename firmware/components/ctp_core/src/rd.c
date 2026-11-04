#include "ctp/rd.h"

#include <string.h>

void ctp_rd_init(ctp_rd_t *r, const uint8_t *p, uint32_t len)
{
    r->p = p;
    r->len = (p == NULL) ? 0u : len;
    r->off = 0u;
    r->err = false;
}

uint32_t ctp_rd_left(const ctp_rd_t *r)
{
    return r->len - r->off;
}

const uint8_t *ctp_rd_ptr(ctp_rd_t *r, uint32_t n)
{
    if (r->err || (r->len - r->off) < n) {
        r->err = true;
        r->off = r->len;
        return NULL;
    }
    const uint8_t *s = &r->p[r->off];
    r->off += n;
    return s;
}

void ctp_rd_bytes(ctp_rd_t *r, uint8_t *dst, uint32_t n)
{
    const uint8_t *s = ctp_rd_ptr(r, n);
    if (s == NULL) {
        memset(dst, 0, n);
    } else {
        memcpy(dst, s, n);
    }
}

uint8_t ctp_rd_u8(ctp_rd_t *r)
{
    const uint8_t *s = ctp_rd_ptr(r, 1u);
    return (s == NULL) ? 0u : s[0];
}

int8_t ctp_rd_i8(ctp_rd_t *r)
{
    uint8_t u = ctp_rd_u8(r);
    return (int8_t)u;
}

uint16_t ctp_rd_u16(ctp_rd_t *r)
{
    const uint8_t *s = ctp_rd_ptr(r, 2u);
    if (s == NULL) {
        return 0u;
    }
    return (uint16_t)((uint16_t)s[0] | (uint16_t)((uint16_t)s[1] << 8));
}

uint32_t ctp_rd_u32(ctp_rd_t *r)
{
    const uint8_t *s = ctp_rd_ptr(r, 4u);
    if (s == NULL) {
        return 0u;
    }
    return (uint32_t)s[0] | ((uint32_t)s[1] << 8) | ((uint32_t)s[2] << 16) | ((uint32_t)s[3] << 24);
}

int32_t ctp_rd_i32(ctp_rd_t *r)
{
    uint32_t u = ctp_rd_u32(r);
    return (int32_t)u;
}

uint64_t ctp_rd_u64(ctp_rd_t *r)
{
    uint64_t lo = ctp_rd_u32(r);
    uint64_t hi = ctp_rd_u32(r);
    return lo | (hi << 32);
}

void ctp_wr_init(ctp_wr_t *w, uint8_t *p, uint32_t cap)
{
    w->p = p;
    w->cap = (p == NULL) ? 0u : cap;
    w->off = 0u;
    w->err = false;
}

void ctp_wr_bytes(ctp_wr_t *w, const uint8_t *src, uint32_t n)
{
    if (w->err || (w->cap - w->off) < n) {
        w->err = true;
        return;
    }
    if (n > 0u) {
        memcpy(&w->p[w->off], src, n);
    }
    w->off += n;
}

void ctp_wr_u8(ctp_wr_t *w, uint8_t v)
{
    ctp_wr_bytes(w, &v, 1u);
}

void ctp_wr_i8(ctp_wr_t *w, int8_t v)
{
    ctp_wr_u8(w, (uint8_t)v);
}

void ctp_wr_u16(ctp_wr_t *w, uint16_t v)
{
    const uint8_t b[2] = {(uint8_t)(v & 0xFFu), (uint8_t)(v >> 8)};
    ctp_wr_bytes(w, b, 2u);
}

void ctp_wr_u32(ctp_wr_t *w, uint32_t v)
{
    const uint8_t b[4] = {(uint8_t)(v & 0xFFu), (uint8_t)((v >> 8) & 0xFFu), (uint8_t)((v >> 16) & 0xFFu),
                          (uint8_t)(v >> 24)};
    ctp_wr_bytes(w, b, 4u);
}

void ctp_wr_i32(ctp_wr_t *w, int32_t v)
{
    ctp_wr_u32(w, (uint32_t)v);
}

void ctp_wr_u64(ctp_wr_t *w, uint64_t v)
{
    ctp_wr_u32(w, (uint32_t)(v & 0xFFFFFFFFu));
    ctp_wr_u32(w, (uint32_t)(v >> 32));
}
