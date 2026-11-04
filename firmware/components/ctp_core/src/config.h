/* Private to ctp_core: config apply helpers used by proto.c. */
#ifndef CTP_CONFIG_PRIV_H
#define CTP_CONFIG_PRIV_H

#include "ctp/ctp.h"

/* Publishes next as the config; bumps cfg_gen only if something changed. */
void cfg_commit(ctp_t *c, const ctp_config_t *next);
CTP_NODISCARD ctp_err_t cfg_set_channel(ctp_t *c, uint8_t ch, uint8_t bw);
CTP_NODISCARD ctp_err_t cfg_set_country(ctp_t *c, const ctp_country_t *req);
CTP_NODISCARD ctp_err_t cfg_set_driver_mask(ctp_t *c, uint32_t type_mask, uint32_t ctrl_mask);
CTP_NODISCARD ctp_err_t cfg_set_capture(ctp_t *c, bool on);
CTP_NODISCARD ctp_err_t cfg_set_filter(ctp_t *c, const uint8_t *body, uint32_t len, bool truncated);

#endif
