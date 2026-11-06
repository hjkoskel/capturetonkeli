/* The only seam between the portable core and a platform (docs/07 §2). */
#ifndef CTP_PORT_H
#define CTP_PORT_H

#include "ctp/common.h"
#include "ctp/wire.h"

typedef struct {
    /* link */
    int32_t (*usb_write)(const uint8_t *buf, uint32_t len, uint32_t timeout_ms); /* bytes written or <0 */
    int32_t (*usb_read)(uint8_t *buf, uint32_t cap, uint32_t timeout_ms);        /* bytes read (0 = timeout) or <0 */
    /* radio. Each returns CTP_OK or a wire error code; the raw platform code
     * is available from last_esp_err() for RSP_ERR.esp_err. */
    /* In/out: on success, fills schan/nchan with the 2.4 GHz range applied. */
    ctp_err_t (*radio_set_country)(ctp_country_t *c);
    /* Validates against the active country table; *applied_bw is what the
     * driver really uses (5 GHz derives the HT40 direction). */
    ctp_err_t (*radio_set_channel)(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied_bw);
    ctp_err_t (*radio_set_driver_mask)(uint32_t type_mask, uint32_t ctrl_mask);
    ctp_err_t (*radio_promisc)(bool on);
    /* system */
    uint64_t (*now_us)(void);
    void (*reboot)(ctp_reboot_mode_t mode); /* may return on host builds (re-init) */
    int32_t (*last_esp_err)(void);
    /* ctrl task wake-up (link → ctrl); wait returns early when notified */
    void (*ctrl_notify)(void);
    void (*ctrl_wait)(uint32_t timeout_ms);
    void (*delay_ms)(uint32_t ms);
} ctp_port_t;

/* Device identity and boot facts provided by the platform at init. */
typedef struct {
    uint32_t fw_version; /* major<<24 | minor<<16 | patch */
    uint8_t fw_flags;
    uint8_t build_id[8];
    uint8_t mac[6];
    uint32_t caps;
    ctp_reset_reason_t reset_reason;
    bool safe_mode;
    char reset_detail[CTP_MAX_DETAIL + 1]; /* "file:line" for ASSERT */
} ctp_boot_info_t;

#endif
