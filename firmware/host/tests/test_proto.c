/* Engine tests with an in-memory mock port (single-threaded: link/ctrl steps
 * are called alternately). The full behaviour is covered by the conformance
 * suite against ctp_fw_host (make fw-host-conf). */
#include <string.h>

#include "ctp/ctp.h"
#include "ctp/frame.h"
#include "ctp/wire.h"
#include "unity.h"

static ctp_t s_c;
static uint8_t s_in[8192];
static uint32_t s_in_len, s_in_off;
static uint8_t s_out[65536];
static uint32_t s_out_len;
static uint64_t s_now;
static int s_reboots;
static uint8_t s_chan;
static bool s_usb_blocked;

static int32_t m_write(const uint8_t *b, uint32_t len, uint32_t t)
{
    (void)t;
    if (s_usb_blocked) {
        return 0;
    }
    if (len > 100u) {
        len = 100u; /* partial writes, like the USJ port */
    }
    if (s_out_len + len > sizeof s_out) {
        len = (uint32_t)sizeof s_out - s_out_len;
    }
    memcpy(&s_out[s_out_len], b, len);
    s_out_len += len;
    return (int32_t)len;
}

static int32_t m_read(uint8_t *b, uint32_t cap, uint32_t t)
{
    (void)t;
    uint32_t n = s_in_len - s_in_off;
    if (n > cap) {
        n = cap;
    }
    memcpy(b, &s_in[s_in_off], n);
    s_in_off += n;
    return (int32_t)n;
}

static ctp_err_t m_country(ctp_country_t *c)
{
    if (c->cc[0] == 'Z') {
        return CTP_E_COUNTRY_INVALID;
    }
    c->schan = 1u;
    c->nchan = 13u;
    return CTP_OK;
}

static ctp_err_t m_channel(uint8_t ch, ctp_bw_t bw, ctp_bw_t *a)
{
    if (ch == 14u) {
        return CTP_E_CHANNEL_NOT_ALLOWED;
    }
    s_chan = ch;
    *a = bw;
    return CTP_OK;
}

static ctp_err_t m_mask(uint32_t a, uint32_t b) { (void)a; (void)b; return CTP_OK; }
static ctp_err_t m_promisc(bool on) { (void)on; return CTP_OK; }
static uint64_t m_now(void) { return s_now; }
static void m_reboot(ctp_reboot_mode_t m) { (void)m; s_reboots++; }
static int32_t m_err(void) { return 0x3004; }
static void m_notify(void) {}
static void m_wait(uint32_t t) { (void)t; }
static void m_delay(uint32_t t) { s_now += (uint64_t)t * 1000u; }

static const ctp_port_t s_port = {m_write, m_read, m_country, m_channel, m_mask, m_promisc, m_now,
                                  m_reboot, m_err, m_notify, m_wait, m_delay};

typedef struct {
    int n;
    ctp_hdr_t h[64];
    uint8_t body[64][256];
    uint32_t len[64];
} frames_t;
static frames_t s_fr;
static ctp_frame_dec_t s_hdec;

static bool grab(void *ctx, const ctp_hdr_t *h, const uint8_t *b, uint32_t len)
{
    (void)ctx;
    if (s_fr.n < 64) {
        s_fr.h[s_fr.n] = *h;
        s_fr.len[s_fr.n] = len;
        memcpy(s_fr.body[s_fr.n], b, (len < 256u) ? len : 256u);
    }
    s_fr.n++;
    return true;
}

static void reset(void)
{
    s_in_len = s_in_off = s_out_len = 0u;
    s_now = 1000000u;
    s_reboots = 0;
    s_usb_blocked = false;
    ctp_boot_info_t boot;
    memset(&boot, 0, sizeof boot);
    boot.fw_version = 0x00010000u;
    boot.caps = CTP_CAP_5GHZ | CTP_CAP_HT40 | CTP_CAP_MANUAL_COUNTRY;
    boot.reset_reason = CTP_RESET_POWER_ON;
    ctp_init(&s_c, &s_port, &boot);
    ctp_frame_dec_init(&s_hdec);
    memset(&s_fr, 0, sizeof s_fr);
}

static void send(uint8_t type, uint16_t id, const uint8_t *body, uint32_t len)
{
    const ctp_hdr_t h = {1u, type, 0u, id, 0u, 0u};
    s_in[s_in_len++] = 0u;
    uint32_t n = 0u;
    TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_frame_encode(&s_in[s_in_len], (uint32_t)sizeof s_in - s_in_len, &h, body, len, &n));
    s_in_len += n;
}

static void pump(int rounds)
{
    for (int i = 0; i < rounds; i++) {
        ctp_link_step(&s_c);
        ctp_ctrl_step(&s_c, 0u);
        ctp_link_step(&s_c);
    }
    (void)ctp_frame_dec_feed(&s_hdec, s_out, s_out_len, grab, NULL);
    s_out_len = 0u;
}

static int find(uint16_t req_id)
{
    for (int i = 0; i < s_fr.n && i < 64; i++) {
        if (s_fr.h[i].req_id == req_id && (s_fr.h[i].type == CTP_T_RSP_OK || s_fr.h[i].type == CTP_T_RSP_ERR)) {
            return i;
        }
    }
    return -1;
}

static void test_boot_reset_event(void)
{
    reset();
    pump(1);
    TEST_ASSERT_TRUE(s_fr.n >= 1);
    TEST_ASSERT_EQUAL_UINT8(CTP_T_EVT_RESET, s_fr.h[0].type);
    TEST_ASSERT_EQUAL_UINT8(CTP_RESET_POWER_ON, s_fr.body[0][0]);
}

static void test_hello_ping_config(void)
{
    reset();
    const uint8_t hello[5] = {0x78, 0x56, 0x34, 0x12, 1};
    send(CTP_T_HELLO, 10u, hello, 5u);
    const uint8_t ping[4] = {1, 0, 0, 0};
    send(CTP_T_PING, 11u, ping, 4u);
    const uint8_t ch[2] = {6, 0};
    send(CTP_T_SET_CHANNEL, 12u, ch, 2u);
    send(CTP_T_SET_CHANNEL, 13u, ch, 2u); /* same value: cfg_gen unchanged */
    const uint8_t bad[2] = {14, 0};
    send(CTP_T_SET_CHANNEL, 14u, bad, 2u);
    send(0x7E, 15u, NULL, 0u);
    pump(3);
    int i = find(10u);
    TEST_ASSERT_TRUE(i >= 0);
    TEST_ASSERT_EQUAL_UINT32(CTP_DEVINFO_LEN, s_fr.len[i]);
    TEST_ASSERT_EQUAL_HEX8(0x78, s_fr.body[i][36]); /* nonce echo */
    TEST_ASSERT_TRUE(find(11u) >= 0);
    i = find(12u);
    TEST_ASSERT_EQUAL_UINT8(CTP_T_RSP_OK, s_fr.h[i].type);
    TEST_ASSERT_EQUAL_UINT8(6u, s_fr.body[i][1]);
    TEST_ASSERT_EQUAL_UINT8(1u, s_fr.body[i][0]);
    TEST_ASSERT_TRUE((s_fr.h[i].flags & CTP_HFLAG_CFG_CHANGED) != 0u);
    TEST_ASSERT_EQUAL_UINT8(1u, s_fr.body[find(13u)][0]);
    i = find(14u);
    TEST_ASSERT_EQUAL_UINT8(CTP_T_RSP_ERR, s_fr.h[i].type);
    TEST_ASSERT_EQUAL_UINT8(CTP_E_CHANNEL_NOT_ALLOWED, s_fr.body[i][0]);
    TEST_ASSERT_EQUAL_UINT8(0x04, s_fr.body[i][2]); /* esp_err from port */
    i = find(15u);
    TEST_ASSERT_EQUAL_UINT8(CTP_E_UNKNOWN_TYPE, s_fr.body[i][0]);
    TEST_ASSERT_EQUAL_UINT8(6u, s_chan);
}

static void test_capture_needs_session_and_watchdog(void)
{
    reset();
    const uint8_t on[1] = {1};
    send(CTP_T_CAPTURE, 20u, on, 1u);
    pump(1);
    int i = find(20u);
    TEST_ASSERT_EQUAL_UINT8(CTP_T_RSP_ERR, s_fr.h[i].type);
    TEST_ASSERT_EQUAL_UINT8(CTP_E_NO_SESSION, s_fr.body[i][0]);

    const uint8_t hello[5] = {1, 2, 3, 4, 1};
    send(CTP_T_HELLO, 21u, hello, 5u);
    send(CTP_T_CAPTURE, 22u, on, 1u);
    pump(2);
    TEST_ASSERT_EQUAL_UINT8(1u, s_fr.body[find(22u)][20]);

    /* a captured frame flows as PKT with the cfg_gen in force */
    uint8_t frame[30] = {0x80};
    ctp_rx_t rx = {.data = frame, .len = 30u, .rssi = -50, .noise = -95, .channel = 6u, .ts_us = 5u};
    ctp_capture_rx(&s_c, &rx);
    memset(&s_fr, 0, sizeof s_fr);
    pump(1);
    TEST_ASSERT_EQUAL_INT(1, s_fr.n);
    TEST_ASSERT_EQUAL_UINT8(CTP_T_PKT, s_fr.h[0].type);
    TEST_ASSERT_EQUAL_UINT32(CTP_PKT_META_LEN + 30u, s_fr.len[0]);

    /* 2.1 s of silence: watchdog ends the session, capture off */
    s_now += 2100000u;
    ctp_capture_rx(&s_c, &rx);
    pump(1);
    TEST_ASSERT_FALSE(atomic_load(&s_c.session));
    TEST_ASSERT_EQUAL_UINT8(0u, s_c.cfg.capture_on);
}

static void test_dedup_and_reboot(void)
{
    reset();
    const uint8_t s1[2] = {77, 0};
    const uint8_t s2[2] = {88, 0};
    send(CTP_T_SET_SNAPLEN, 30u, s1, 2u);
    send(CTP_T_SET_SNAPLEN, 31u, s2, 2u);
    send(CTP_T_SET_SNAPLEN, 30u, s1, 2u); /* duplicate */
    pump(2);
    TEST_ASSERT_EQUAL_UINT16(88u, s_c.cfg.snaplen);
    const uint8_t rb[1] = {0};
    send(CTP_T_REBOOT, 32u, rb, 1u);
    pump(2);
    TEST_ASSERT_TRUE(find(32u) >= 0);
    TEST_ASSERT_EQUAL_INT(1, s_reboots);
}

static void test_filter_publish(void)
{
    reset();
    const uint8_t beacons[] = {1, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 1, 0, 0x80, 0xfc};
    send(CTP_T_SET_FILTER, 40u, beacons, (uint32_t)sizeof beacons);
    const uint8_t bad[] = {1, 0, 0, 0, 0, 0, 0, 0, 0};
    send(CTP_T_SET_FILTER, 41u, bad, (uint32_t)sizeof bad);
    pump(2);
    TEST_ASSERT_EQUAL_UINT8(1u, s_c.cfg.n_rules);
    TEST_ASSERT_EQUAL_UINT8(CTP_E_FILTER_INVALID, s_fr.body[find(41u)][0]);
    TEST_ASSERT_EQUAL_UINT32(1u, atomic_load(&s_c.active_filter));
}

static void test_stalled_host_does_not_wedge_link(void)
{
    reset();
    s_usb_blocked = true; /* host stops reading */
    const uint8_t ping[4] = {1, 0, 0, 0};
    send(CTP_T_PING, 50u, ping, 4u);
    pump(2);
    s_now += 300000u; /* > CTP_TX_STALL_MS */
    pump(1);
    s_usb_blocked = false;
    const uint8_t rb[1] = {0};
    send(CTP_T_REBOOT, 51u, rb, 1u);
    pump(3);
    TEST_ASSERT_TRUE(find(51u) >= 0);
    TEST_ASSERT_EQUAL_INT(1, s_reboots);
}

void run_proto_tests(void)
{
    RUN_TEST(test_stalled_host_does_not_wedge_link);
    RUN_TEST(test_boot_reset_event);
    RUN_TEST(test_hello_ping_config);
    RUN_TEST(test_capture_needs_session_and_watchdog);
    RUN_TEST(test_dedup_and_reboot);
    RUN_TEST(test_filter_publish);
}
