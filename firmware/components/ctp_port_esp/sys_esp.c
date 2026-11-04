/* System: time, reboot, reset reason + crash-loop guard, assert record in
 * no-init RAM, app descriptor → version/build id (docs/07 §4, docs/10 §3). */
#include <stdio.h>
#include <string.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_app_desc.h"
#include "esp_attr.h"
#include "esp_mac.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "soc/lp_aon_reg.h"
#include "soc/soc.h"

#include "ctp_port_esp.h"

#define NOINIT_MAGIC 0x43544B31u /* "CTK1" */
#define CRASH_LOOP_N 3u

typedef struct {
    uint32_t magic;
    uint32_t crash_count;
    uint32_t assert_pending;
    uint32_t assert_line;
    char assert_file[40];
} noinit_t;

static __NOINIT_ATTR noinit_t s_noinit;
static TaskHandle_t s_ctrl_task;
static bool s_uptime_cleared;

static const char *basename_of(const char *p)
{
    const char *b = p;
    for (const char *s = p; *s != '\0'; s++) {
        if (*s == '/') {
            b = s + 1;
        }
    }
    return b;
}

void ctp_assert_fail(const char *file, int line)
{
    if (s_noinit.magic != NOINIT_MAGIC) {
        memset(&s_noinit, 0, sizeof s_noinit);
        s_noinit.magic = NOINIT_MAGIC;
    }
    s_noinit.assert_pending = 1u;
    s_noinit.assert_line = (uint32_t)line;
    snprintf(s_noinit.assert_file, sizeof s_noinit.assert_file, "%s", basename_of(file));
    esp_restart();
}

static uint32_t parse_version(const char *v, uint8_t *flags)
{
    unsigned ma = 0, mi = 0, pa = 0;
    if (sscanf(v, "%u.%u.%u", &ma, &mi, &pa) != 3) {
        ma = 0;
        mi = 0;
        pa = 0;
    }
    *flags = 0u;
    if (strstr(v, "-dirty") != NULL) {
        *flags |= 0x01u;
    }
    if (strstr(v, "-dev") != NULL) {
        *flags |= 0x04u;
    }
    return ((uint32_t)(ma & 0xFFu) << 24) | ((uint32_t)(mi & 0xFFu) << 16) | (pa & 0xFFFFu);
}

void ctk_sys_boot_info(ctp_boot_info_t *boot)
{
    memset(boot, 0, sizeof *boot);
    if (s_noinit.magic != NOINIT_MAGIC) {
        memset(&s_noinit, 0, sizeof s_noinit);
        s_noinit.magic = NOINIT_MAGIC;
    }
    const esp_app_desc_t *d = esp_app_get_description();
    boot->fw_version = parse_version(d->version, &boot->fw_flags);
    memcpy(boot->build_id, d->app_elf_sha256, sizeof boot->build_id);
    (void)esp_read_mac(boot->mac, ESP_MAC_WIFI_STA);
    boot->caps = CTP_CAP_5GHZ; /* no HT40 in NULL mode (docs/10 §2, M3) */
    if (ctk_radio_manual_country_supported()) {
        boot->caps |= CTP_CAP_MANUAL_COUNTRY;
    }

    bool abnormal = true;
    switch (esp_reset_reason()) {
    case ESP_RST_POWERON:
        boot->reset_reason = CTP_RESET_POWER_ON;
        abnormal = false;
        break;
    case ESP_RST_SW:
        boot->reset_reason = CTP_RESET_SOFTWARE;
        abnormal = false;
        break;
    case ESP_RST_PANIC:
    case ESP_RST_CPU_LOCKUP:
        boot->reset_reason = CTP_RESET_PANIC;
        break;
    case ESP_RST_INT_WDT:
    case ESP_RST_TASK_WDT:
    case ESP_RST_WDT:
        boot->reset_reason = CTP_RESET_WATCHDOG;
        break;
    case ESP_RST_BROWNOUT:
    case ESP_RST_PWR_GLITCH:
        boot->reset_reason = CTP_RESET_BROWNOUT;
        break;
    case ESP_RST_EXT:
    case ESP_RST_USB:
    case ESP_RST_JTAG:
        boot->reset_reason = CTP_RESET_EXTERNAL;
        abnormal = false;
        break;
    default:
        boot->reset_reason = CTP_RESET_UNKNOWN;
        break;
    }
    if (s_noinit.assert_pending != 0u) {
        boot->reset_reason = CTP_RESET_ASSERT;
        snprintf(boot->reset_detail, sizeof boot->reset_detail, "%s:%u", s_noinit.assert_file,
                 (unsigned)s_noinit.assert_line);
        s_noinit.assert_pending = 0u;
        abnormal = true;
    }
    s_noinit.crash_count = abnormal ? s_noinit.crash_count + 1u : 0u;
    boot->safe_mode = s_noinit.crash_count >= CRASH_LOOP_N;
}

void ctk_sys_uptime_tick(void)
{
    if (!s_uptime_cleared && esp_timer_get_time() > 60LL * 1000000LL) {
        s_noinit.crash_count = 0u; /* 60 s of stable uptime ends a crash loop */
        s_uptime_cleared = true;
    }
}

void ctk_sys_set_ctrl_task(void *task_handle)
{
    s_ctrl_task = (TaskHandle_t)task_handle;
}

uint64_t ctk_sys_now_us(void)
{
    return (uint64_t)esp_timer_get_time();
}

void ctk_sys_reboot(ctp_reboot_mode_t mode)
{
    if (mode == CTP_REBOOT_NORMAL) {
        s_noinit.crash_count = 0u;
        esp_restart();
    }
    /* REBOOT 1 (docs/10 §3, verified in M3): force ROM "download boot0"
     * (UART/USB-Serial-JTAG) for the next boot, then software-reset. This
     * avoids the host DTR/RTS sequence, which proved unreliable on hardware.
     * The USB PHY is not touched. */
    (void)esp_wifi_set_promiscuous(false);
    s_noinit.crash_count = 0u;
    REG_SET_FIELD(LP_AON_SYS_CFG_REG, LP_AON_FORCE_DOWNLOAD_BOOT, 1u);
    esp_restart();
}

void ctk_sys_ctrl_notify(void)
{
    if (s_ctrl_task != NULL) {
        xTaskNotifyGive(s_ctrl_task);
    }
}

void ctk_sys_ctrl_wait(uint32_t timeout_ms)
{
    (void)ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(timeout_ms));
}

void ctk_sys_delay_ms(uint32_t ms)
{
    const TickType_t t = pdMS_TO_TICKS(ms);
    vTaskDelay(t == 0u ? 1u : t);
}
