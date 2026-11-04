/* USB Serial/JTAG transport (docs/01, docs/10 §3). */
#include "freertos/FreeRTOS.h" /* v6: driver headers no longer include FreeRTOS */
#include "driver/usb_serial_jtag.h"

#include "ctp_port_esp.h"

static TickType_t ticks(uint32_t ms)
{
    const TickType_t t = pdMS_TO_TICKS(ms);
    return (ms > 0u && t == 0u) ? 1u : t;
}

void ctk_usb_init(void)
{
    usb_serial_jtag_driver_config_t cfg = {
        .tx_buffer_size = 8192u, /* tuned in M3 */
        .rx_buffer_size = 4096u,
    };
    ESP_ERROR_CHECK(usb_serial_jtag_driver_install(&cfg));
}

int32_t ctk_usb_write(const uint8_t *buf, uint32_t len, uint32_t timeout_ms)
{
    if (!usb_serial_jtag_is_connected()) {
        return 0; /* no host: never block (avoids the 50 ms USJ stall) */
    }
    const int n = usb_serial_jtag_write_bytes(buf, len, ticks(timeout_ms));
    return (int32_t)n;
}

int32_t ctk_usb_read(uint8_t *buf, uint32_t cap, uint32_t timeout_ms)
{
    const int n = usb_serial_jtag_read_bytes(buf, cap, ticks(timeout_ms));
    return (int32_t)n;
}
