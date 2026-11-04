/* Lock-free single-producer/single-consumer record ring (C11 atomics).
 * Producer: promiscuous callback. Consumer: link task. Records are whole
 * or absent: a full ring rejects the new record (never half a frame). */
#ifndef CTP_RING_H
#define CTP_RING_H

#include <stdatomic.h>

#include "ctp/common.h"

typedef struct {
    uint8_t *buf;
    uint32_t size;        /* multiple of 4 */
    _Atomic uint32_t head; /* written by producer */
    _Atomic uint32_t tail; /* written by consumer */
    /* producer-private reservation */
    uint32_t res_off;
    uint32_t res_need;
    bool res_wrap;
} ctp_ring_t;

void ctp_ring_init(ctp_ring_t *r, uint8_t *buf, uint32_t size);

/* Producer: reserve len payload bytes; NULL if full. Then commit. */
uint8_t *ctp_ring_reserve(ctp_ring_t *r, uint32_t len);
void ctp_ring_commit(ctp_ring_t *r);

/* Consumer: oldest record or NULL; release after use. */
const uint8_t *ctp_ring_peek(ctp_ring_t *r, uint32_t *len);
void ctp_ring_release(ctp_ring_t *r);
/* Consumer: drop everything currently queued. */
void ctp_ring_flush(ctp_ring_t *r);

/* Fill level 0..100 (either side; approximate). */
uint16_t ctp_ring_fill_pct(ctp_ring_t *r);

#endif
