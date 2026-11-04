/* Wi-Fi radio: NULL mode, promiscuous, band/channel/bw, country (docs/02 §4–5, docs/10 §2).
 * Zero flash writes: NVS off, RAM storage, country never persisted. */
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "esp_event.h"
#include "esp_wifi.h"

#include "ctp_port_esp.h"

static int32_t s_last_err;
static ctp_t *s_core;

static ctp_err_t radio_err(esp_err_t e, ctp_err_t mapped)
{
    s_last_err = (int32_t)e;
    return (e == ESP_OK) ? CTP_OK : mapped;
}

void ctk_radio_init(ctp_t *core, bool start_radio)
{
    s_core = core;
    if (!start_radio) {
        return; /* safe mode: radio stays off (docs/07 §4) */
    }
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    cfg.nvs_enable = 0;
    ESP_ERROR_CHECK(esp_wifi_init(&cfg)); /* exactly once (v6: second call fails) */
    ESP_ERROR_CHECK(esp_wifi_set_storage(WIFI_STORAGE_RAM));
    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_NULL));
    ESP_ERROR_CHECK(esp_wifi_start());
    ESP_ERROR_CHECK(esp_wifi_set_ps(WIFI_PS_NONE));
    ESP_ERROR_CHECK(esp_wifi_set_country_code("01", false));
    ESP_ERROR_CHECK(esp_wifi_set_band_mode(WIFI_BAND_MODE_2G_ONLY));
    ESP_ERROR_CHECK(esp_wifi_set_channel(1, WIFI_SECOND_CHAN_NONE));
    const wifi_promiscuous_filter_t f = {.filter_mask = WIFI_PROMIS_FILTER_MASK_ALL};
    ESP_ERROR_CHECK(esp_wifi_set_promiscuous_filter(&f));
    const wifi_promiscuous_filter_t cf = {.filter_mask = WIFI_PROMIS_CTRL_FILTER_MASK_ALL};
    ESP_ERROR_CHECK(esp_wifi_set_promiscuous_ctrl_filter(&cf));
    ctk_cb_attach(core);
}

int32_t ctk_radio_last_err(void)
{
    return s_last_err;
}

bool ctk_radio_manual_country_supported(void)
{
    return true; /* legacy esp_wifi_set_country present in v6.0.2 (idf_api_smoke) */
}

ctp_err_t ctk_radio_set_country(ctp_country_t *c)
{
    esp_err_t e;
    if (c->mode == 0u) {
        const char cc[3] = {c->cc[0], c->cc[1], '\0'};
        e = esp_wifi_set_country_code(cc, false);
        if (e != ESP_OK) {
            return radio_err(e, CTP_E_COUNTRY_INVALID);
        }
    } else {
        wifi_country_t wc;
        memset(&wc, 0, sizeof wc);
        wc.cc[0] = c->cc[0];
        wc.cc[1] = c->cc[1];
        wc.cc[2] = ' ';
        wc.schan = c->schan;
        wc.nchan = c->nchan;
        wc.policy = WIFI_COUNTRY_POLICY_MANUAL;
        e = esp_wifi_set_country(&wc);
        if (e != ESP_OK) {
            return radio_err(e, CTP_E_COUNTRY_INVALID);
        }
    }
    wifi_country_t got;
    e = esp_wifi_get_country(&got);
    if (e != ESP_OK) {
        return radio_err(e, CTP_E_RADIO);
    }
    c->schan = got.schan;
    c->nchan = got.nchan;
    return radio_err(ESP_OK, CTP_OK);
}

ctp_err_t ctk_radio_set_channel(uint8_t ch, ctp_bw_t bw, ctp_bw_t *applied)
{
    const bool five = ch > 14u;
    /* band first, never AUTO (docs/10 §2) */
    esp_err_t e = esp_wifi_set_band_mode(five ? WIFI_BAND_MODE_5G_ONLY : WIFI_BAND_MODE_2G_ONLY);
    if (e != ESP_OK) {
        return radio_err(e, CTP_E_RADIO);
    }
    wifi_second_chan_t second = WIFI_SECOND_CHAN_NONE;
    ctp_bw_t a = CTP_BW20;
    if (bw != CTP_BW20) {
        /* Open question (docs/10 §2): bandwidth in NULL mode. Fall back to HT20
         * and report it in the readback if the driver refuses. */
        if (esp_wifi_set_bandwidth(WIFI_IF_STA, WIFI_BW40) == ESP_OK) {
            if (five) {
                const uint32_t idx = (ch >= 149u) ? ((uint32_t)ch - 149u) / 4u : ((uint32_t)ch - 36u) / 4u;
                a = ((idx % 2u) == 0u) ? CTP_BW40_PLUS : CTP_BW40_MINUS;
            } else {
                a = bw;
                second = (bw == CTP_BW40_PLUS) ? WIFI_SECOND_CHAN_ABOVE : WIFI_SECOND_CHAN_BELOW;
            }
        }
    } else {
        (void)esp_wifi_set_bandwidth(WIFI_IF_STA, WIFI_BW20);
    }
    e = esp_wifi_set_channel(ch, second);
    if (e != ESP_OK) {
        return radio_err(e, CTP_E_CHANNEL_NOT_ALLOWED);
    }
    *applied = a;
    return radio_err(ESP_OK, CTP_OK);
}

ctp_err_t ctk_radio_set_driver_mask(uint32_t type_mask, uint32_t ctrl_mask)
{
    const wifi_promiscuous_filter_t f = {.filter_mask = type_mask};
    esp_err_t e = esp_wifi_set_promiscuous_filter(&f);
    if (e == ESP_OK) {
        const wifi_promiscuous_filter_t cf = {.filter_mask = ctrl_mask};
        e = esp_wifi_set_promiscuous_ctrl_filter(&cf);
    }
    return radio_err(e, CTP_E_RADIO);
}

ctp_err_t ctk_radio_promisc(bool on)
{
    return radio_err(esp_wifi_set_promiscuous(on), CTP_E_RADIO);
}
