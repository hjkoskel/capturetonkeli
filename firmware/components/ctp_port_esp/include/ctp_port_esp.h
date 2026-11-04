/* ESP-IDF port for ctp_core. */
#ifndef CTP_PORT_ESP_H
#define CTP_PORT_ESP_H

#include "ctp/ctp.h"

/* usb_usj.c */
void ctk_usb_init(void);
int32_t ctk_usb_write(const uint8_t *buf, uint32_t len, uint32_t timeout_ms);
int32_t ctk_usb_read(uint8_t *buf, uint32_t cap, uint32_t timeout_ms);

/* radio_esp.c */
void ctk_radio_init(ctp_t *core, bool start_radio);
ctp_err_t ctk_radio_set_country(ctp_country_t *c);
ctp_err_t ctk_radio_set_channel(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied);
ctp_err_t ctk_radio_set_driver_mask(uint32_t type_mask, uint32_t ctrl_mask);
ctp_err_t ctk_radio_promisc(bool on);
int32_t ctk_radio_last_err(void);
bool ctk_radio_manual_country_supported(void);

/* cb_esp.c */
void ctk_cb_attach(ctp_t *core);
void ctk_diag_report(char *buf, size_t cap); /* CONFIG_CTK_DIAG only */

/* synth_esp.c (CONFIG_CTK_SYNTHETIC_TRAFFIC only) */
void ctk_synth_start(ctp_t *core);

/* sys_esp.c */
void ctk_sys_boot_info(ctp_boot_info_t *boot);
void ctk_sys_set_ctrl_task(void *task_handle);
uint64_t ctk_sys_now_us(void);
void ctk_sys_reboot(ctp_reboot_mode_t mode);
void ctk_sys_ctrl_notify(void);
void ctk_sys_ctrl_wait(uint32_t timeout_ms);
void ctk_sys_delay_ms(uint32_t ms);
void ctk_sys_uptime_tick(void); /* clears the crash-loop counter after 60 s */

#endif
