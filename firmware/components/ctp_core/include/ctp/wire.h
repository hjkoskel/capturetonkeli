/* Message bodies (docs/03 §6.1, §9). Decoders accept longer bodies; a short
 * mandatory field yields CTP_E_BAD_BODY. Encoders return the body length. */
#ifndef CTP_WIRE_H
#define CTP_WIRE_H

#include "ctp/common.h"

typedef struct { uint32_t host_nonce; uint8_t host_proto_ver; } ctp_hello_t;
typedef struct { uint8_t channel; uint8_t bw; } ctp_set_channel_t;
typedef struct { char cc[3]; uint8_t mode; uint8_t schan; uint8_t nchan; } ctp_country_t;
typedef struct { uint32_t type_mask; uint32_t ctrl_mask; } ctp_driver_mask_t;

typedef struct {
    uint8_t proto_ver;
    uint32_t fw_version;
    uint8_t fw_flags;
    uint8_t build_id[8];
    uint8_t mac[6];
    uint32_t caps;
    uint16_t max_frame;
    uint16_t max_rules;
    uint64_t device_time_us;
    uint32_t host_nonce;
    uint8_t reset_reason;
    uint8_t state_flags;
} ctp_devinfo_t;
#define CTP_DEVINFO_LEN 42u

typedef struct {
    uint8_t cfg_gen;
    uint8_t channel;
    uint8_t bw;
    uint8_t band;
    ctp_country_t country;
    uint16_t snaplen;
    uint32_t type_mask;
    uint32_t ctrl_mask;
    uint8_t capture_on;
    uint8_t n_rules;
    uint32_t rules_crc32;
} ctp_config_t;
#define CTP_CONFIG_LEN 26u

typedef struct {
    uint64_t device_time_us;
    uint32_t rx_total, rx_passed, drop_ring, drop_fcs, tx_frames, tx_bytes, host_rx_crc_err, host_rx_oversize;
    uint16_t ring_high_water;
    uint8_t channel;
    int8_t noise_floor;
    uint32_t flash_writes;
} ctp_stats_t;
#define CTP_STATS_LEN 48u

typedef struct {
    uint64_t ts_us;
    uint8_t channel;
    uint8_t flags;
    int8_t rssi;
    int8_t noise;
    uint8_t rate;
    uint8_t bb_format;
    uint16_t orig_len;
    uint16_t cap_len;
    uint16_t rx_seq;
} ctp_pkt_meta_t;
#define CTP_PKT_META_LEN 20u

/* requests */
CTP_NODISCARD ctp_err_t ctp_dec_hello(const uint8_t *b, uint32_t len, ctp_hello_t *m);
CTP_NODISCARD ctp_err_t ctp_dec_u8(const uint8_t *b, uint32_t len, uint8_t *v);   /* CAPTURE, GET_STATS, REBOOT */
CTP_NODISCARD ctp_err_t ctp_dec_u16(const uint8_t *b, uint32_t len, uint16_t *v); /* SET_SNAPLEN */
CTP_NODISCARD ctp_err_t ctp_dec_u32(const uint8_t *b, uint32_t len, uint32_t *v); /* PING */
CTP_NODISCARD ctp_err_t ctp_dec_set_channel(const uint8_t *b, uint32_t len, ctp_set_channel_t *m);
CTP_NODISCARD ctp_err_t ctp_dec_country(const uint8_t *b, uint32_t len, ctp_country_t *m);
CTP_NODISCARD ctp_err_t ctp_dec_driver_mask(const uint8_t *b, uint32_t len, ctp_driver_mask_t *m);

/* responses / events: return bytes written (0 if cap too small) */
uint32_t ctp_enc_devinfo(uint8_t *dst, uint32_t cap, const ctp_devinfo_t *m);
uint32_t ctp_enc_pong(uint8_t *dst, uint32_t cap, uint32_t host_time_ms, uint64_t device_time_us);
uint32_t ctp_enc_config(uint8_t *dst, uint32_t cap, const ctp_config_t *m);
uint32_t ctp_enc_stats(uint8_t *dst, uint32_t cap, const ctp_stats_t *m);
uint32_t ctp_enc_rsp_err(uint8_t *dst, uint32_t cap, uint16_t code, int32_t esp_err, const char *detail);
uint32_t ctp_enc_reset(uint8_t *dst, uint32_t cap, uint8_t reason, uint32_t fw_version, uint8_t state_flags, const char *detail);
uint32_t ctp_enc_log(uint8_t *dst, uint32_t cap, uint8_t level, const char *text);
uint32_t ctp_enc_pkt_meta(uint8_t *dst, uint32_t cap, const ctp_pkt_meta_t *m);

/* Rate conversion from C5 rx_ctrl (docs/03 §9 rev 2). raw_rate is the
 * 11b rate code or the L-SIG RATE field. Sets *unknown when not mappable. */
uint8_t ctp_rate_from_rxctrl(uint8_t bb_format, uint8_t raw_rate, bool *unknown);

#endif
