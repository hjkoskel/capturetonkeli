/* Throughput test variant (CONFIG_CTK_SYNTHETIC_TRAFFIC, docs/07 M3):
 * synthetic frames through the real capture path. */
#include "sdkconfig.h"

#if CONFIG_CTK_SYNTHETIC_TRAFFIC
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_timer.h"

#include "ctp_port_esp.h"

static ctp_t *s_core;
static uint8_t s_frame[CONFIG_CTK_SYNTH_FRAME_LEN];

static void synth_task(void *arg)
{
    (void)arg;
    /* QoS data frame: FC 88 02, then a counter in the body */
    memset(s_frame, 0xA5, sizeof s_frame);
    s_frame[0] = 0x88;
    s_frame[1] = 0x02;
    uint32_t n = 0;
    for (;;) {
        for (int i = 0; i < 64; i++) {
            memcpy(&s_frame[24], &n, sizeof n);
            n++;
            const ctp_rx_t rx = {
                .data = s_frame, .len = (uint16_t)sizeof s_frame, .rssi = -40, .noise = -95,
                .bb_format = 1u, .rate = 108u, .channel = 6u, .no_fcs = true, .ts_us = (uint64_t)esp_timer_get_time(),
            };
            ctp_capture_rx(s_core, &rx);
        }
        vTaskDelay(1); /* let IDLE run (task WDT) */
    }
}

void ctk_synth_start(ctp_t *core)
{
    s_core = core;
    xTaskCreate(synth_task, "ctk_synth", 3072, NULL, 1, NULL);
}
#endif
