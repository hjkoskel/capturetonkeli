/*
 * ctp_core common definitions. PORTABLE: no ESP-IDF / FreeRTOS headers.
 * Must compile warning-free as C11 and C23 (docs/07 §4, docs/10 §1).
 */
#ifndef CTP_COMMON_H
#define CTP_COMMON_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

/* ---- protocol constants (docs/03) ---------------------------------------- */
#define CTP_PROTO_VER      1u
#define CTP_HDR_LEN        8u
#define CTP_CRC_LEN        4u
#define CTP_MAX_FRAME      (4096u + 32u)          /* header + body + crc */
#define CTP_MAX_BODY       (CTP_MAX_FRAME - CTP_HDR_LEN - CTP_CRC_LEN)
#define CTP_MAX_CAP_LEN    4096u
#define CTP_COBS_MAX(n)    ((n) + (n) / 254u + 2u)
#define CTP_MAX_RULES      16u
#define CTP_MAX_TERMS      4u
#define CTP_MAX_TERM_LEN   16u
#define CTP_MAX_DETAIL     64u
#define CTP_DEDUP_N        4u
#define CTP_EMPTY_FILTER_CRC 0x6522DF69u

/* message types */
enum {
    CTP_T_HELLO = 0x01, CTP_T_PING = 0x02, CTP_T_GET_CONFIG = 0x10, CTP_T_SET_CHANNEL = 0x11,
    CTP_T_SET_COUNTRY = 0x12, CTP_T_SET_FILTER = 0x13, CTP_T_SET_SNAPLEN = 0x14,
    CTP_T_SET_DRIVER_MASK = 0x15, CTP_T_CAPTURE = 0x20, CTP_T_GET_STATS = 0x21, CTP_T_REBOOT = 0x30,
    CTP_T_RSP_OK = 0x81, CTP_T_RSP_ERR = 0x82, CTP_T_EVT_STATS = 0x90, CTP_T_EVT_LOG = 0x91,
    CTP_T_EVT_RESET = 0x92, CTP_T_PKT = 0xA0
};

#define CTP_HFLAG_CFG_CHANGED 0x02u

#define CTP_CAP_5GHZ           (1u << 0)
#define CTP_CAP_HT40           (1u << 1)
#define CTP_CAP_MANUAL_COUNTRY (1u << 2)

#define CTP_PKT_FCS_ERR      0x01u
#define CTP_PKT_TRUNCATED    0x02u
#define CTP_PKT_5GHZ         0x04u
#define CTP_PKT_HT40         0x08u
#define CTP_PKT_AMPDU        0x10u
#define CTP_PKT_RATE_UNKNOWN 0x20u

#define CTP_STATE_SAFE_MODE  0x01u

#define CTP_DEFAULT_TYPE_MASK 0xFFFFFFFFu
#define CTP_DEFAULT_CTRL_MASK 0xFF800000u

/*
 * Result codes. Values 1..10 are the wire err_code (docs/03 §6.1) so they can
 * be sent unchanged in RSP_ERR. Values >= 100 are internal.
 */
typedef enum {
    CTP_OK = 0,
    CTP_E_UNKNOWN_TYPE = 1,
    CTP_E_BAD_BODY = 2,
    CTP_E_INVALID_ARG = 3,
    CTP_E_CHANNEL_NOT_ALLOWED = 4,
    CTP_E_COUNTRY_INVALID = 5,
    CTP_E_NOT_SUPPORTED = 6,
    CTP_E_FILTER_INVALID = 7,
    CTP_E_NO_SESSION = 8,
    CTP_E_RADIO = 9,
    CTP_E_SAFE_MODE = 10,
    CTP_E_FULL = 100,
    CTP_E_EMPTY = 101,
    CTP_E_OVERFLOW = 102,
    CTP_E_COBS = 103
} ctp_err_t;

typedef enum { CTP_BW20 = 0, CTP_BW40_PLUS = 1, CTP_BW40_MINUS = 2 } ctp_bw_t;
typedef enum { CTP_REBOOT_NORMAL = 0, CTP_REBOOT_DOWNLOAD = 1 } ctp_reboot_mode_t;

typedef enum {
    CTP_RESET_UNKNOWN = 0, CTP_RESET_POWER_ON = 1, CTP_RESET_SOFTWARE = 2, CTP_RESET_PANIC = 3,
    CTP_RESET_WATCHDOG = 4, CTP_RESET_BROWNOUT = 5, CTP_RESET_ASSERT = 6, CTP_RESET_EXTERNAL = 7
} ctp_reset_reason_t;

#if defined(__GNUC__)
#define CTP_NODISCARD __attribute__((__warn_unused_result__))
#define CTP_NORETURN  __attribute__((__noreturn__))
#else
#define CTP_NODISCARD
#define CTP_NORETURN
#endif

/* C11 _Static_assert; never the C23 keyword static_assert (docs/10 §1). */
#define CTP_STATIC_ASSERT(cond, msg) _Static_assert(cond, msg)

/*
 * Always-on assertion (docs/07 §4 rule 9). The handler is provided by the
 * platform: it records file:line in no-init RAM and reboots.
 */
CTP_NORETURN void ctp_assert_fail(const char *file, int line);
#define CTP_ASSERT(cond) do { if (!(cond)) { ctp_assert_fail(__FILE__, __LINE__); } } while (0)

#endif
