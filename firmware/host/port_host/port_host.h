/* Host (PC) port for ctp_fw_host: PTY transport, fake radio, POSIX system. */
#ifndef CTP_PORT_HOST_H
#define CTP_PORT_HOST_H

#include "ctp/ctp.h"

/* usb_pty.c */
int usb_pty_open(const char *link_path); /* prints the slave path; returns 0 on success */
void usb_pty_set_rate(uint32_t bytes_per_s);
int32_t usb_pty_write(const uint8_t *buf, uint32_t len, uint32_t timeout_ms);
int32_t usb_pty_read(uint8_t *buf, uint32_t cap, uint32_t timeout_ms);

/* radio_fake.c */
void radio_fake_init(ctp_t *core, uint32_t seed, uint32_t frames_per_s);
void radio_fake_reset(void);       /* boot state (country 01, ch 1, promisc off) */
void radio_fake_tick(void);        /* generate frames for the elapsed time (radio thread) */
ctp_err_t radio_fake_set_country(ctp_country_t *c);
ctp_err_t radio_fake_set_channel(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied);
ctp_err_t radio_fake_set_driver_mask(uint32_t type_mask, uint32_t ctrl_mask);
ctp_err_t radio_fake_promisc(bool on);
int32_t radio_fake_last_err(void);

/* sys_host.c */
uint64_t sys_host_now_us(void);
void sys_host_reboot(ctp_reboot_mode_t mode);
bool sys_host_take_reboot(ctp_reboot_mode_t *mode);
void sys_host_ctrl_notify(void);
void sys_host_ctrl_wait(uint32_t timeout_ms);
void sys_host_delay_ms(uint32_t ms);

#endif
