/* Fuzz: SET_FILTER decoder + matcher on the same bytes as a frame. */
#include "ctp/filter.h"

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

static ctp_filter_t s_f;

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size)
{
    const uint32_t n = (uint32_t)(size > 8192u ? 8192u : size);
    if (ctp_filter_decode(data, n, CTP_MAX_RULES, &s_f) == CTP_OK) {
        const ctp_match_meta_t m = {.fcs_err = (n & 1u) != 0u, .rssi = -50, .orig_len = (uint16_t)n};
        (void)ctp_filter_match(&s_f, data, n, &m);
    }
    return 0;
}
