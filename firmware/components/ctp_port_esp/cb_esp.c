/* Promiscuous rx callback → ctp_capture_rx (docs/02 §1, docs/10 §2 C5 rx_ctrl). */
#include "freertos/FreeRTOS.h"
#include "esp_timer.h"
#include "esp_wifi.h"

#include "ctp_port_esp.h"

static ctp_t *s_core;

static void IRAM_ATTR on_rx(void *buf, wifi_promiscuous_pkt_type_t type)
{
    if (type == WIFI_PKT_MISC) {
        return;
    }
    const wifi_promiscuous_pkt_t *p = (const wifi_promiscuous_pkt_t *)buf;
    const wifi_pkt_rx_ctrl_t *rc = &p->rx_ctrl;
    /* Fields are bitfields: read by name only (docs/10 §2). */
    uint32_t len = rc->sig_len; /* incl. FCS */
    const uint32_t dump = rc->dump_len;
    if (dump != 0u && dump < len && (len - dump) != 4u) {
        len = dump; /* driver truncated the payload (verify in M4, docs/08 A1) */
    }
    ctp_rx_t rx = {
        .data = p->payload,
        .len = (uint16_t)(len > 0xFFFFu ? 0xFFFFu : len),
        .rssi = (int8_t)rc->rssi,
        .noise = (int8_t)rc->noise_floor,
        .bb_format = (uint8_t)rc->cur_bb_format,
        .channel = (uint8_t)rc->channel,
        .ht40 = rc->second != 0u,
        .fcs_err = rc->rx_state != 0u,
        .ampdu = false, /* not reported by the C5 rx_ctrl */
        .ts_us = (uint64_t)esp_timer_get_time(),
    };
    rx.rate = ctp_rate_from_rxctrl(rx.bb_format, (uint8_t)rc->rate, &rx.rate_unknown);
    ctp_capture_rx(s_core, &rx);
}

void ctk_cb_attach(ctp_t *core)
{
    s_core = core;
    ESP_ERROR_CHECK(esp_wifi_set_promiscuous_rx_cb(on_rx));
}
