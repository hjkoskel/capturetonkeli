#define _XOPEN_SOURCE 700 /* posix_openpt, ptsname */
/* PTY transport: the host firmware looks like a serial dongle to ctpbench. */
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <termios.h>
#include <unistd.h>

#include "port_host.h"

static int s_master = -1;
static int s_slave_hold = -1;
static uint32_t s_rate;  /* bytes/s, 0 = unlimited */
static double s_tokens;
static uint64_t s_last_us;

static int make_raw(int fd)
{
    struct termios t;
    if (tcgetattr(fd, &t) != 0) {
        return -1;
    }
    cfmakeraw(&t);
    return tcsetattr(fd, TCSANOW, &t);
}

int usb_pty_open(const char *link_path)
{
    s_master = posix_openpt(O_RDWR | O_NOCTTY);
    if (s_master < 0 || grantpt(s_master) != 0 || unlockpt(s_master) != 0) {
        perror("posix_openpt");
        return -1;
    }
    const char *name = ptsname(s_master);
    if (name == NULL) {
        return -1;
    }
    char path[128];
    snprintf(path, sizeof path, "%s", name);
    /* Hold the slave open so the master never sees EIO between clients, and
     * put it in raw mode so no line discipline touches our bytes. */
    s_slave_hold = open(path, O_RDWR | O_NOCTTY);
    if (s_slave_hold < 0 || make_raw(s_slave_hold) != 0) {
        perror("pty slave");
        return -1;
    }
    (void)fcntl(s_master, F_SETFL, fcntl(s_master, F_GETFL) | O_NONBLOCK);
    if (link_path != NULL) {
        (void)unlink(link_path);
        if (symlink(path, link_path) != 0) {
            perror("symlink");
            return -1;
        }
    }
    printf("%s\n", path);
    fflush(stdout);
    return 0;
}

void usb_pty_set_rate(uint32_t bytes_per_s)
{
    s_rate = bytes_per_s;
}

int32_t usb_pty_read(uint8_t *buf, uint32_t cap, uint32_t timeout_ms)
{
    struct pollfd p = {.fd = s_master, .events = POLLIN};
    const int r = poll(&p, 1, (int)timeout_ms);
    if (r <= 0) {
        return 0;
    }
    const ssize_t n = read(s_master, buf, cap);
    if (n < 0) {
        if (errno == EAGAIN || errno == EINTR || errno == EIO) {
            return 0;
        }
        return -1;
    }
    return (int32_t)n;
}

int32_t usb_pty_write(const uint8_t *buf, uint32_t len, uint32_t timeout_ms)
{
    /* model the USB Full-Speed bottleneck with a token bucket */
    if (s_rate != 0u) {
        const uint64_t now = sys_host_now_us();
        if (s_last_us != 0u) {
            s_tokens += (double)(now - s_last_us) * (double)s_rate / 1e6;
        }
        s_last_us = now;
        if (s_tokens > 8192.0) {
            s_tokens = 8192.0;
        }
        if (s_tokens < 64.0) {
            sys_host_delay_ms(1u);
            return 0;
        }
        if ((double)len > s_tokens) {
            len = (uint32_t)s_tokens;
        }
        s_tokens -= (double)len;
    }
    uint32_t done = 0u;
    const uint64_t deadline = sys_host_now_us() + (uint64_t)timeout_ms * 1000u;
    while (done < len) {
        const ssize_t n = write(s_master, &buf[done], len - done);
        if (n > 0) {
            done += (uint32_t)n;
            continue;
        }
        if (n < 0 && errno != EAGAIN && errno != EINTR) {
            return (done > 0u) ? (int32_t)done : -1;
        }
        if (sys_host_now_us() >= deadline) {
            break;
        }
        struct pollfd p = {.fd = s_master, .events = POLLOUT};
        (void)poll(&p, 1, 1);
    }
    return (int32_t)done;
}
