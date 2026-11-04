/* Fuzz: stream decoder (resync, COBS, CRC, oversize). */
#include "ctp/frame.h"

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

static ctp_frame_dec_t s_dec;

static bool cb(void *ctx, const ctp_hdr_t *h, const uint8_t *body, uint32_t len)
{
    (void)ctx;
    (void)h;
    CTP_ASSERT(len <= CTP_MAX_BODY);
    volatile uint8_t x = 0u;
    for (uint32_t i = 0u; i < len; i++) {
        x = (uint8_t)(x ^ body[i]);
    }
    return true;
}

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size)
{
    ctp_frame_dec_init(&s_dec);
    const uint32_t n = (uint32_t)(size > 0x100000u ? 0x100000u : size);
    CTP_ASSERT(ctp_frame_dec_feed(&s_dec, data, n, cb, NULL) == n);
    return 0;
}
