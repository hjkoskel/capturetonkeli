/* Promiscuous rx callback → ctp_capture_rx (docs/02 §1, docs/10 §2 C5 rx_ctrl). */
#include "freertos/FreeRTOS.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "sdkconfig.h"
#if CONFIG_CTK_DIAG
#include <stdio.h>
#include "esp_cpu.h"
#endif

#include "ctp_port_esp.h"

static ctp_t *s_core;

#if CONFIG_CTK_DIAG
/* written only by the Wi-Fi task; read approximately by the ctrl task */
static volatile uint32_t s_type[4][4]; /* [driver type][FC type] */
static volatile uint32_t s_cyc_max, s_cyc_n;
static volatile uint64_t s_cyc_sum;
#endif

static void IRAM_ATTR on_rx(void *buf, wifi_promiscuous_pkt_type_t type)
{
#if !CONFIG_CTK_DIAG
    if (type == WIFI_PKT_MISC) {
        return;
    }
#endif
    const wifi_promiscuous_pkt_t *p = (const wifi_promiscuous_pkt_t *)buf;
    const wifi_pkt_rx_ctrl_t *rc = &p->rx_ctrl;
    /* Fields are bitfields: read by name only (docs/10 §2). */
    /* M3 finding (IDF v6.0.2): sig_len counts the FCS, but the last 4 bytes in
     * payload are driver data, not the FCS (dump_len = sig_len + 4). Strip them
     * and flag no_fcs (docs/03 §9 rev 3). */
    uint32_t len = rc->sig_len;
    len = (len >= 4u) ? len - 4u : 0u;
    ctp_rx_t rx = {
        .data = p->payload,
        .len = (uint16_t)(len > 0xFFFFu ? 0xFFFFu : len),
        .rssi = (int8_t)rc->rssi,
        .noise = (int8_t)rc->noise_floor,
        .bb_format = (uint8_t)rc->cur_bb_format,
        .channel = (uint8_t)rc->channel,
        .ht40 = rc->second != 0u,
        .no_fcs = true,
        .fcs_err = rc->rx_state != 0u,
        .ampdu = false, /* not reported by the C5 rx_ctrl */
        .ts_us = (uint64_t)esp_timer_get_time(),
    };
    rx.rate = ctp_rate_from_rxctrl(rx.bb_format, (uint8_t)rc->rate, &rx.rate_unknown);
#if CONFIG_CTK_DIAG
    if (len >= 1u && (uint32_t)type < 4u && rc->rx_state == 0u) { /* good FCS only */
        s_type[(uint32_t)type][(p->payload[0] >> 2) & 3u]++;
    }
    const uint32_t c0 = esp_cpu_get_cycle_count();
    ctp_capture_rx(s_core, &rx);
    const uint32_t dc = esp_cpu_get_cycle_count() - c0;
    if (dc > s_cyc_max) {
        s_cyc_max = dc;
    }
    s_cyc_sum += dc;
    s_cyc_n++;
#else
    ctp_capture_rx(s_core, &rx);
#endif
}

#if CONFIG_CTK_DIAG
void ctk_diag_report(char *buf, size_t cap)
{
    const uint32_t n = s_cyc_n;
    const uint32_t avg = n ? (uint32_t)(s_cyc_sum / n) : 0u;
    const uint32_t mhz = (uint32_t)CONFIG_ESP_DEFAULT_CPU_FREQ_MHZ;
    int o = snprintf(buf, cap, "diag cb n=%lu avg=%lu cyc (%lu ns) max=%lu cyc (%lu ns); type[drv][fc]:",
                     (unsigned long)n, (unsigned long)avg, (unsigned long)(avg * 1000u / mhz),
                     (unsigned long)s_cyc_max, (unsigned long)(s_cyc_max * 1000u / mhz));
    for (int d = 0; d < 4 && o > 0 && (size_t)o < cap; d++) {
        o += snprintf(buf + o, cap - (size_t)o, " %d:[%lu %lu %lu %lu]", d, (unsigned long)s_type[d][0],
                      (unsigned long)s_type[d][1], (unsigned long)s_type[d][2], (unsigned long)s_type[d][3]);
    }
    s_cyc_max = 0u; /* max per report window */
}
#endif

void ctk_cb_attach(ctp_t *core)
{
    s_core = core;
    ESP_ERROR_CHECK(esp_wifi_set_promiscuous_rx_cb(on_rx));
}
