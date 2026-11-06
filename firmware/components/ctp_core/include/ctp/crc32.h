/* CRC-32 IEEE (reflected, zlib/Ethernet), table-driven (docs/03 §3). */
#ifndef CTP_CRC32_H
#define CTP_CRC32_H

#include "ctp/common.h"

/* Incremental: crc = ctp_crc32_update(0, ...) chains like zlib's crc32(). */
uint32_t ctp_crc32_update(uint32_t crc, const uint8_t *p, uint32_t len);
uint32_t ctp_crc32(const uint8_t *p, uint32_t len);
/* Builds the table; call once before tasks start (ctp_init does). */
void ctp_crc32_init(void);

#endif
