/* Counters (docs/03 §8). Written with relaxed atomics from callback/link,
 * read (and reset) by the ctrl task. */
#include "ctp/ctp.h"

static uint32_t take(_Atomic uint32_t *v, bool reset)
{
    return reset ? atomic_exchange_explicit(v, 0u, memory_order_relaxed) : atomic_load_explicit(v, memory_order_relaxed);
}

void ctp_stats_snapshot(ctp_t *c, ctp_stats_t *st, bool reset)
{
    ctp_counters_t *k = &c->cnt;
    st->device_time_us = c->port->now_us();
    st->rx_total = take(&k->rx_total, reset);
    st->rx_passed = take(&k->rx_passed, reset);
    st->drop_ring = take(&k->drop_ring, reset);
    st->drop_fcs = take(&k->drop_fcs, reset);
    st->tx_frames = take(&k->tx_frames, reset);
    st->tx_bytes = take(&k->tx_bytes, reset);
    st->host_rx_crc_err = take(&k->host_crc, reset);
    st->host_rx_oversize = take(&k->host_oversize, reset);
    /* high water is always "max since last read" */
    st->ring_high_water = (uint16_t)atomic_exchange_explicit(&k->high_water, 0u, memory_order_relaxed);
    st->channel = c->cfg.channel;
    st->noise_floor = (int8_t)atomic_load_explicit(&k->noise, memory_order_relaxed);
    st->flash_writes = 0u; /* the core never writes flash (docs/07 §4) */
}
