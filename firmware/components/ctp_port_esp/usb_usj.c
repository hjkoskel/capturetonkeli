/* USB Serial/JTAG transport (docs/01, docs/10 §3). */
#include "freertos/FreeRTOS.h" /* v6: driver headers no longer include FreeRTOS */
#include "driver/usb_serial_jtag.h"

#include "ctp_port_esp.h"

#define USJ_TX_BUF   1024u
#define USJ_TX_CHUNK (USJ_TX_BUF / 2u) /* the driver never accepts a write larger than its buffer */

static TickType_t ticks(uint32_t ms)
{
    const TickType_t t = pdMS_TO_TICKS(ms);
    return (ms > 0u && t == 0u) ? 1u : t; /* 0 ms = poll */
}

void ctk_usb_init(void)
{
    usb_serial_jtag_driver_config_t cfg = {
        /* M3: every byte queued here delays control responses under load
         * (962 KB/s link: 8 KB ≈ 8.5 ms). 1 KB + 512 B batches: PING p99 < 5 ms at full load. */
        .tx_buffer_size = USJ_TX_BUF,
        .rx_buffer_size = 4096u,
    };
    ESP_ERROR_CHECK(usb_serial_jtag_driver_install(&cfg));
}

int32_t ctk_usb_write(const uint8_t *buf, uint32_t len, uint32_t timeout_ms)
{
    if (!usb_serial_jtag_is_connected()) {
        return 0; /* no host: never block (avoids the 50 ms USJ stall) */
    }
    if (len > USJ_TX_CHUNK) {
        len = USJ_TX_CHUNK; /* partial write; the core resumes from tx_off */
    }
    const int n = usb_serial_jtag_write_bytes(buf, len, ticks(timeout_ms));
    return (int32_t)n;
}

int32_t ctk_usb_read(uint8_t *buf, uint32_t cap, uint32_t timeout_ms)
{
    const int n = usb_serial_jtag_read_bytes(buf, cap, ticks(timeout_ms));
    return (int32_t)n;
}
