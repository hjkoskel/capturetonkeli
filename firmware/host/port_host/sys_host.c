#include <pthread.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

#include "port_host.h"

static pthread_mutex_t s_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t s_cv = PTHREAD_COND_INITIALIZER;
static bool s_notified;
static _Atomic int s_reboot = -1;

uint64_t sys_host_now_us(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 1000000u + (uint64_t)ts.tv_nsec / 1000u;
}

void sys_host_reboot(ctp_reboot_mode_t mode)
{
    /* Host "reboot" = re-initialise the core in-process; the PTY stays. */
    atomic_store(&s_reboot, (int)mode);
}

bool sys_host_take_reboot(ctp_reboot_mode_t *mode)
{
    const int m = atomic_exchange(&s_reboot, -1);
    if (m < 0) {
        return false;
    }
    *mode = (m == 1) ? CTP_REBOOT_DOWNLOAD : CTP_REBOOT_NORMAL;
    return true;
}

void sys_host_ctrl_notify(void)
{
    pthread_mutex_lock(&s_mu);
    s_notified = true;
    pthread_cond_signal(&s_cv);
    pthread_mutex_unlock(&s_mu);
}

void sys_host_ctrl_wait(uint32_t timeout_ms)
{
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_sec += (time_t)(timeout_ms / 1000u);
    ts.tv_nsec += (long)(timeout_ms % 1000u) * 1000000L;
    if (ts.tv_nsec >= 1000000000L) {
        ts.tv_sec++;
        ts.tv_nsec -= 1000000000L;
    }
    pthread_mutex_lock(&s_mu);
    while (!s_notified) {
        if (pthread_cond_timedwait(&s_cv, &s_mu, &ts) != 0) {
            break;
        }
    }
    s_notified = false;
    pthread_mutex_unlock(&s_mu);
}

void sys_host_delay_ms(uint32_t ms)
{
    struct timespec ts = {.tv_sec = (time_t)(ms / 1000u), .tv_nsec = (long)(ms % 1000u) * 1000000L};
    nanosleep(&ts, NULL);
}

void ctp_assert_fail(const char *file, int line)
{
    fprintf(stderr, "ctp_fw_host: CTP_ASSERT failed at %s:%d\n", file, line);
    abort();
}
