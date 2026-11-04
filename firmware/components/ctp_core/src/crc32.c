#include "ctp/crc32.h"

/* Table generated at compile time would need a constexpr; a 1 KB const table
 * is computed once by ctp_crc32_update's first call instead (no heap). */
static uint32_t s_table[256];
static bool s_ready;

void ctp_crc32_init(void)
{
    for (uint32_t i = 0u; i < 256u; i++) {
        uint32_t c = i;
        for (uint32_t k = 0u; k < 8u; k++) {
            c = ((c & 1u) != 0u) ? (0xEDB88320u ^ (c >> 1)) : (c >> 1);
        }
        s_table[i] = c;
    }
    s_ready = true;
}

uint32_t ctp_crc32_update(uint32_t crc, const uint8_t *p, uint32_t len)
{
    if (!s_ready) {
        ctp_crc32_init();
    }
    uint32_t c = crc ^ 0xFFFFFFFFu;
    for (uint32_t i = 0u; i < len; i++) {
        c = s_table[(c ^ p[i]) & 0xFFu] ^ (c >> 8);
    }
    return c ^ 0xFFFFFFFFu;
}

uint32_t ctp_crc32(const uint8_t *p, uint32_t len)
{
    return ctp_crc32_update(0u, p, len);
}
