/* Fuzz: every request body decoder (sticky-error cursors). */
#include "ctp/wire.h"

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size)
{
    const uint32_t n = (uint32_t)(size > 4096u ? 4096u : size);
    ctp_hello_t h;
    ctp_set_channel_t sc;
    ctp_country_t c;
    ctp_driver_mask_t dm;
    uint8_t u8 = 0u;
    uint16_t u16 = 0u;
    uint32_t u32 = 0u;
    uint32_t bad = 0u; /* gcc ignores (void) on warn_unused_result */
    bad += (ctp_dec_hello(data, n, &h) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_set_channel(data, n, &sc) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_country(data, n, &c) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_driver_mask(data, n, &dm) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_u8(data, n, &u8) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_u16(data, n, &u16) != CTP_OK) ? 1u : 0u;
    bad += (ctp_dec_u32(data, n, &u32) != CTP_OK) ? 1u : 0u;
    return (int)(bad & 0u);
}
