/*
 * ctp_fw_host: the real firmware core with host ports, a dongle on a PTY
 * (docs/07 §2, M2). Three threads mirror the firmware tasks:
 * link (USB), ctrl (requests/timers) and radio (fake promiscuous callback).
 *
 *   ctp_fw_host [--link PATH] [--seed N] [--fps N] [--usb-rate BYTES_PER_S]
 *               [--reset-reason N] [--safe-mode] [--no-5g] [--no-manual]
 */
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "port_host.h"

static ctp_t s_core;
static _Atomic bool s_run;
static _Atomic bool s_quit;

static ctp_err_t p_set_country(ctp_country_t *c) { return radio_fake_set_country(c); }

static const ctp_port_t s_port = {
    .usb_write = usb_pty_write,
    .usb_read = usb_pty_read,
    .radio_set_country = p_set_country,
    .radio_set_channel = radio_fake_set_channel,
    .radio_set_driver_mask = radio_fake_set_driver_mask,
    .radio_promisc = radio_fake_promisc,
    .now_us = sys_host_now_us,
    .reboot = sys_host_reboot,
    .last_esp_err = radio_fake_last_err,
    .ctrl_notify = sys_host_ctrl_notify,
    .ctrl_wait = sys_host_ctrl_wait,
    .delay_ms = sys_host_delay_ms,
};

static void *link_thread(void *arg)
{
    (void)arg;
    while (atomic_load(&s_run)) {
        ctp_link_step(&s_core);
    }
    return NULL;
}

static void *ctrl_thread(void *arg)
{
    (void)arg;
    while (atomic_load(&s_run)) {
        ctp_ctrl_step(&s_core, 10u);
    }
    return NULL;
}

static void *radio_thread(void *arg)
{
    (void)arg;
    while (atomic_load(&s_run)) {
        radio_fake_tick();
        sys_host_delay_ms(1u);
    }
    return NULL;
}

static void on_signal(int sig)
{
    (void)sig;
    atomic_store(&s_quit, true);
}

static void usage(void)
{
    fprintf(stderr, "usage: ctp_fw_host [--link PATH] [--seed N] [--fps N] [--usb-rate B/s] [--reset-reason N] "
                    "[--safe-mode] [--no-5g] [--no-manual]\n");
}

int main(int argc, char **argv)
{
    const char *link = NULL;
    uint32_t seed = 1u;
    uint32_t fps = 400u;
    uint32_t rate = 1000000u;
    ctp_boot_info_t boot;
    memset(&boot, 0, sizeof boot);
    boot.fw_version = (0u << 24) | (1u << 16) | 0u; /* 0.1.0 dev */
    boot.fw_flags = 0x04u;                          /* dev build */
    const uint8_t mac[6] = {0x02, 0xc5, 0x00, 0x00, 0x00, 0x02};
    memcpy(boot.mac, mac, sizeof mac);
    memcpy(boot.build_id, "hostfw00", 8u);
    boot.caps = CTP_CAP_5GHZ | CTP_CAP_HT40 | CTP_CAP_MANUAL_COUNTRY;
    boot.reset_reason = CTP_RESET_POWER_ON;

    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        const char *v = (i + 1 < argc) ? argv[i + 1] : NULL;
        if (strcmp(a, "--link") == 0 && v != NULL) {
            link = v;
            i++;
        } else if (strcmp(a, "--seed") == 0 && v != NULL) {
            seed = (uint32_t)strtoul(v, NULL, 0);
            i++;
        } else if (strcmp(a, "--fps") == 0 && v != NULL) {
            fps = (uint32_t)strtoul(v, NULL, 0);
            i++;
        } else if (strcmp(a, "--usb-rate") == 0 && v != NULL) {
            rate = (uint32_t)strtoul(v, NULL, 0);
            i++;
        } else if (strcmp(a, "--reset-reason") == 0 && v != NULL) {
            boot.reset_reason = (ctp_reset_reason_t)strtoul(v, NULL, 0);
            i++;
        } else if (strcmp(a, "--safe-mode") == 0) {
            boot.safe_mode = true;
        } else if (strcmp(a, "--no-5g") == 0) {
            boot.caps &= ~CTP_CAP_5GHZ;
        } else if (strcmp(a, "--no-manual") == 0) {
            boot.caps &= ~CTP_CAP_MANUAL_COUNTRY;
        } else {
            usage();
            return 2;
        }
    }
    signal(SIGINT, on_signal);
    signal(SIGTERM, on_signal);
    signal(SIGPIPE, SIG_IGN);
    if (usb_pty_open(link) != 0) {
        return 1;
    }
    usb_pty_set_rate(rate);
    radio_fake_init(&s_core, seed, fps);

    for (;;) {
        ctp_init(&s_core, &s_port, &boot);
        atomic_store(&s_run, true);
        pthread_t tl, tc, tr;
        pthread_create(&tl, NULL, link_thread, NULL);
        pthread_create(&tc, NULL, ctrl_thread, NULL);
        pthread_create(&tr, NULL, radio_thread, NULL);
        ctp_reboot_mode_t mode = CTP_REBOOT_NORMAL;
        while (!atomic_load(&s_quit) && !sys_host_take_reboot(&mode)) {
            sys_host_delay_ms(5u);
        }
        atomic_store(&s_run, false);
        sys_host_ctrl_notify();
        pthread_join(tl, NULL);
        pthread_join(tc, NULL);
        pthread_join(tr, NULL);
        if (atomic_load(&s_quit)) {
            break;
        }
        fprintf(stderr, "ctp_fw_host: reboot (%s)\n", mode == CTP_REBOOT_DOWNLOAD ? "download" : "normal");
        radio_fake_reset();
        boot.reset_reason = CTP_RESET_SOFTWARE;
        boot.reset_detail[0] = '\0';
    }
    if (link != NULL) {
        (void)unlink(link);
    }
    return 0;
}
