/*
 * Protocol engine (docs/07 §3). One ctp_t instance, statically allocated by
 * the platform, driven by three contexts:
 *   - link task:  ctp_link_step() in a loop (USB RX → requests; ctrl-out
 *                 queue first, then packet ring → USB TX)
 *   - ctrl task:  ctp_ctrl_step() in a loop (requests, config, timers)
 *   - Wi-Fi task: ctp_capture_rx() from the promiscuous callback
 * Exactly one writer per piece of state; cross-context data is atomic or
 * passes through SPSC queues.
 */
#ifndef CTP_CTP_H
#define CTP_CTP_H

#include <stdatomic.h>

#include "ctp/common.h"
#include "ctp/filter.h"
#include "ctp/frame.h"
#include "ctp/port.h"
#include "ctp/ring.h"
#include "ctp/wire.h"

#ifndef CTP_RING_SIZE
#define CTP_RING_SIZE (96u * 1024u)
#endif
#define CTP_TX_BUF       4608u  /* ≥ one max PKT frame; docs/07 §3 said 2 KB, revisited in M3 */
#define CTP_RX_CHUNK     512u
#define CTP_REQ_SLOTS    4u
#define CTP_REQ_BODY_MAX 2400u  /* > max SET_FILTER (2328); longer bodies are truncated (flagged) */
#define CTP_OUT_SLOTS    8u
#define CTP_OUT_BODY_MAX 208u   /* largest: EVT_LOG 201 */
#define CTP_WATCHDOG_MS  2000u
#define CTP_STATS_US     1000000u

CTP_STATIC_ASSERT(CTP_RING_SIZE <= 128u * 1024u, "packet ring ≤ 128 KB (docs/07 §3)");
CTP_STATIC_ASSERT((CTP_RING_SIZE % 4u) == 0u, "ring size multiple of 4");
CTP_STATIC_ASSERT(CTP_REQ_BODY_MAX >= 2328u, "max SET_FILTER body fits a request slot");

/* SPSC slot queue indices (producer writes head, consumer writes tail). */
typedef struct {
    _Atomic uint32_t head;
    _Atomic uint32_t tail;
} ctp_q_t;

typedef struct {
    ctp_hdr_t h;
    uint16_t len;
    bool truncated;
    uint8_t body[CTP_REQ_BODY_MAX];
} ctp_req_slot_t;

typedef struct {
    uint8_t type;
    uint16_t req_id;
    uint8_t cfg_gen;
    uint16_t len;
    uint8_t body[CTP_OUT_BODY_MAX];
} ctp_out_slot_t;

typedef struct {
    bool valid;
    uint16_t req_id;
    uint8_t type;
    uint32_t crc;
    uint8_t rtype;
    uint16_t rlen;
    uint8_t rbody[CTP_OUT_BODY_MAX];
} ctp_dedup_t;

typedef struct {
    _Atomic uint32_t rx_total, rx_passed, drop_ring, drop_fcs, tx_frames, tx_bytes, host_crc, host_oversize;
    _Atomic uint32_t high_water;
    _Atomic int32_t noise;
} ctp_counters_t;

/* One received frame as handed over by the platform callback. */
typedef struct {
    const uint8_t *data;  /* 802.11 frame from FC byte 0, incl. FCS */
    uint16_t len;         /* sig_len (bytes available in data) */
    int8_t rssi;
    int8_t noise;
    uint8_t bb_format;
    uint8_t rate;         /* already converted (ctp_rate_from_rxctrl) */
    bool rate_unknown;
    uint8_t channel;
    bool ht40;
    bool fcs_err;
    bool ampdu;
    uint64_t ts_us;
} ctp_rx_t;

typedef enum { CTP_RB_NONE = 0, CTP_RB_NORMAL = 1, CTP_RB_DOWNLOAD = 2 } ctp_rb_req_t;

typedef struct {
    const ctp_port_t *port;
    ctp_boot_info_t boot;

    /* ---- ctrl task owned ---- */
    ctp_config_t cfg;
    ctp_dedup_t dedup[CTP_DEDUP_N];
    uint32_t dedup_next;
    uint64_t next_stats_us;
    ctp_filter_t filters[2];

    /* ---- shared (atomic) ---- */
    _Atomic uint32_t active_filter;
    _Atomic uint32_t cb_epoch;   /* odd while the callback runs */
    _Atomic bool session;
    _Atomic bool capture_on;
    _Atomic uint32_t snaplen;
    _Atomic uint32_t cfg_gen;
    _Atomic uint32_t last_rx_ms;
    _Atomic bool flush_req;
    _Atomic bool rxseq_reset;
    _Atomic uint32_t reboot_req;  /* ctp_rb_req_t */
    ctp_counters_t cnt;

    /* ---- callback owned ---- */
    uint16_t rx_seq;

    /* ---- link task owned ---- */
    ctp_frame_dec_t dec;
    ctp_dec_stats_t dec_seen;
    uint16_t tx_seq;
    uint8_t last_gen_tx;
    bool first_tx;
    uint32_t tx_len;
    uint32_t tx_off;
    uint8_t txbuf[CTP_TX_BUF];
    uint8_t rxbuf[CTP_RX_CHUNK];
    uint32_t rx_len; /* bytes in rxbuf */
    uint32_t rx_off; /* bytes already fed to the decoder */

    /* ---- queues & ring ---- */
    ctp_q_t reqq;
    ctp_req_slot_t req[CTP_REQ_SLOTS];
    ctp_q_t outq;
    ctp_out_slot_t out[CTP_OUT_SLOTS];
    ctp_ring_t ring;
    uint8_t ringbuf[CTP_RING_SIZE] __attribute__((aligned(4)));
} ctp_t;

/* Boot: capture off, country "01", no streaming until HELLO; queues EVT_RESET. */
void ctp_init(ctp_t *c, const ctp_port_t *port, const ctp_boot_info_t *boot);

/* Link task body (one iteration, blocks ≤ ~1 ms in usb_read + ≤ 5 ms in usb_write). */
void ctp_link_step(ctp_t *c);

/* Ctrl task body (one iteration, waits ≤ timeout_ms for a request). */
void ctp_ctrl_step(ctp_t *c, uint32_t timeout_ms);

/* Promiscuous callback body. Never blocks, no logging (docs/07 §4 rule 10). */
void ctp_capture_rx(ctp_t *c, const ctp_rx_t *rx);

/* Queue an EVT_LOG from the ctrl context (debug only). */
void ctp_log(ctp_t *c, uint8_t level, const char *text);

/* ---- internal, exposed for unit tests ---- */
void ctp_config_boot(ctp_config_t *cfg);
bool ctp_config_same(const ctp_config_t *a, const ctp_config_t *b);
void ctp_stats_snapshot(ctp_t *c, ctp_stats_t *st, bool reset);

#endif
