#include "ctp/ring.h"

#include <string.h>

#define REC_HDR  4u
#define WRAP_MARK 0xFFFFFFFFu

static uint32_t align4(uint32_t v)
{
    return (v + 3u) & ~3u;
}

void ctp_ring_init(ctp_ring_t *r, uint8_t *buf, uint32_t size)
{
    CTP_ASSERT((size % 4u) == 0u && size >= 64u && size <= (4u << 20));
    r->buf = buf;
    r->size = size;
    atomic_store_explicit(&r->head, 0u, memory_order_relaxed);
    atomic_store_explicit(&r->tail, 0u, memory_order_relaxed);
    r->res_off = 0u;
    r->res_need = 0u;
    r->res_wrap = false;
}

static void put_u32(uint8_t *p, uint32_t v)
{
    memcpy(p, &v, sizeof v);
}

static uint32_t get_u32(const uint8_t *p)
{
    uint32_t v;
    memcpy(&v, p, sizeof v);
    return v;
}

uint8_t *ctp_ring_reserve(ctp_ring_t *r, uint32_t len)
{
    const uint32_t need = align4(REC_HDR + len);
    const uint32_t h = atomic_load_explicit(&r->head, memory_order_relaxed);
    const uint32_t t = atomic_load_explicit(&r->tail, memory_order_acquire);
    if (need >= r->size) {
        return NULL;
    }
    r->res_wrap = false;
    if (h >= t) {
        const uint32_t room = r->size - h;
        if (room >= need && !((h + need) == r->size && t == 0u)) {
            r->res_off = h;
        } else if (need < t) {
            r->res_off = 0u;
            r->res_wrap = true;
        } else {
            return NULL;
        }
    } else {
        if ((t - h) <= need) {
            return NULL;
        }
        r->res_off = h;
    }
    r->res_need = need;
    put_u32(&r->buf[r->res_off], len);
    return &r->buf[r->res_off + REC_HDR];
}

void ctp_ring_commit(ctp_ring_t *r)
{
    if (r->res_wrap) {
        const uint32_t h = atomic_load_explicit(&r->head, memory_order_relaxed);
        put_u32(&r->buf[h], WRAP_MARK);
    }
    uint32_t nh = r->res_off + r->res_need;
    if (nh == r->size) {
        nh = 0u;
    }
    atomic_store_explicit(&r->head, nh, memory_order_release);
}

const uint8_t *ctp_ring_peek(ctp_ring_t *r, uint32_t *len)
{
    uint32_t t = atomic_load_explicit(&r->tail, memory_order_relaxed);
    const uint32_t h = atomic_load_explicit(&r->head, memory_order_acquire);
    if (t == h) {
        return NULL;
    }
    uint32_t l = get_u32(&r->buf[t]);
    if (l == WRAP_MARK) {
        t = 0u;
        atomic_store_explicit(&r->tail, 0u, memory_order_release);
        if (t == h) {
            return NULL;
        }
        l = get_u32(&r->buf[0]);
    }
    CTP_ASSERT(l < r->size);
    *len = l;
    return &r->buf[t + REC_HDR];
}

void ctp_ring_release(ctp_ring_t *r)
{
    const uint32_t t = atomic_load_explicit(&r->tail, memory_order_relaxed);
    const uint32_t l = get_u32(&r->buf[t]);
    uint32_t nt = t + align4(REC_HDR + l);
    if (nt == r->size) {
        nt = 0u;
    }
    atomic_store_explicit(&r->tail, nt, memory_order_release);
}

void ctp_ring_flush(ctp_ring_t *r)
{
    const uint32_t h = atomic_load_explicit(&r->head, memory_order_acquire);
    atomic_store_explicit(&r->tail, h, memory_order_release);
}

uint16_t ctp_ring_fill_pct(ctp_ring_t *r)
{
    const uint32_t h = atomic_load_explicit(&r->head, memory_order_acquire);
    const uint32_t t = atomic_load_explicit(&r->tail, memory_order_acquire);
    const uint32_t used = (h >= t) ? (h - t) : (r->size - t + h);
    /* 32-bit math (no __udivdi3 on RV32): used ≤ 128 KB, ×100 fits */
    return (uint16_t)(used * 100u / r->size);
}
