/* Composition root: init order and task creation only, no logic (docs/07 §2). */
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "esp_task_wdt.h"
#include "sdkconfig.h"

#include "ctp/ctp.h"
#include "ctp_port_esp.h"

#define LINK_STACK 4096u
#define CTRL_STACK 4096u
#define LINK_PRIO  10u
#define CTRL_PRIO  5u

static ctp_t s_core;
static StaticTask_t s_link_tcb, s_ctrl_tcb;
static StackType_t s_link_stack[LINK_STACK], s_ctrl_stack[CTRL_STACK];

static ctp_err_t set_country(ctp_country_t *c) { return ctk_radio_set_country(c); }

static const ctp_port_t s_port = {
    .usb_write = ctk_usb_write,
    .usb_read = ctk_usb_read,
    .radio_set_country = set_country,
    .radio_set_channel = ctk_radio_set_channel,
    .radio_set_driver_mask = ctk_radio_set_driver_mask,
    .radio_promisc = ctk_radio_promisc,
    .now_us = ctk_sys_now_us,
    .reboot = ctk_sys_reboot,
    .last_esp_err = ctk_radio_last_err,
    .ctrl_notify = ctk_sys_ctrl_notify,
    .ctrl_wait = ctk_sys_ctrl_wait,
    .delay_ms = ctk_sys_delay_ms,
};

static void link_task(void *arg)
{
    (void)arg;
    ESP_ERROR_CHECK(esp_task_wdt_add(NULL));
    for (;;) {
        ctp_link_step(&s_core);
        (void)esp_task_wdt_reset();
    }
}

static void ctrl_task(void *arg)
{
    (void)arg;
    ESP_ERROR_CHECK(esp_task_wdt_add(NULL));
    for (;;) {
        ctp_ctrl_step(&s_core, 20u);
        ctk_sys_uptime_tick();
        (void)esp_task_wdt_reset();
    }
}

void app_main(void)
{
    ctp_boot_info_t boot;
    ctk_sys_boot_info(&boot);
#if CONFIG_CTK_SYNTHETIC_TRAFFIC
    boot.caps = 0u; /* test variant: no radio, synthetic frames (docs/07 M3) */
    const bool radio = false;
#else
    const bool radio = !boot.safe_mode;
#endif
    ctk_usb_init();
    ctp_init(&s_core, &s_port, &boot);
    ctk_radio_init(&s_core, radio);
#if CONFIG_CTK_SYNTHETIC_TRAFFIC
    ctk_synth_start(&s_core);
#endif
    TaskHandle_t ctrl = xTaskCreateStatic(ctrl_task, "ctp_ctrl", CTRL_STACK, NULL, CTRL_PRIO, s_ctrl_stack, &s_ctrl_tcb);
    ctk_sys_set_ctrl_task(ctrl);
    (void)xTaskCreateStatic(link_task, "ctp_link", LINK_STACK, NULL, LINK_PRIO, s_link_stack, &s_link_tcb);
}
