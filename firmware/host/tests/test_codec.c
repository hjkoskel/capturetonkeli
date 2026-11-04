/* rd/cobs/crc32/frame/wire against the shared Go vectors (testvectors/c). */
#include <string.h>

#include "ctp/cobs.h"
#include "ctp/ctp.h"
#include "ctp/crc32.h"
#include "ctp/frame.h"
#include "ctp/rd.h"
#include "ctp/wire.h"
#include "tv_cobs.h"
#include "tv_crc32.h"
#include "tv_frames.h"
#include "tv_messages.h"
#include "unity.h"

static uint8_t buf[CTP_ENC_BUF + 64];
static uint8_t buf2[CTP_MAX_FRAME + 64];

static void test_cobs_vectors(void)
{
    for (size_t i = 0; i < TV_COBS_N; i++) {
        const tv_cobs_t *v = &tv_cobs[i];
        uint32_t n = 0u;
        TEST_ASSERT_EQUAL_INT_MESSAGE(CTP_OK, ctp_cobs_encode(v->in, (uint32_t)v->in_len, buf, sizeof buf, &n), v->name);
        TEST_ASSERT_EQUAL_UINT32_MESSAGE(v->out_len, n, v->name);
        TEST_ASSERT_EQUAL_MEMORY_MESSAGE(v->out, buf, n, v->name);
        TEST_ASSERT_EQUAL_INT_MESSAGE(CTP_OK, ctp_cobs_decode(v->out, (uint32_t)v->out_len, buf2, sizeof buf2, &n), v->name);
        TEST_ASSERT_EQUAL_UINT32_MESSAGE(v->in_len, n, v->name);
        if (n > 0u) {
            TEST_ASSERT_EQUAL_MEMORY_MESSAGE(v->in, buf2, n, v->name);
        }
        /* streaming encoder in two pieces gives the same bytes */
        ctp_cobs_enc_t e;
        ctp_cobs_enc_begin(&e, buf, sizeof buf);
        const uint32_t half = (uint32_t)v->in_len / 2u;
        ctp_cobs_enc_put(&e, v->in, half);
        ctp_cobs_enc_put(&e, (v->in != NULL) ? &v->in[half] : NULL, (uint32_t)v->in_len - half);
        TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_cobs_enc_end(&e, &n));
        TEST_ASSERT_EQUAL_MEMORY_MESSAGE(v->out, buf, n, v->name);
    }
}

static void test_cobs_errors(void)
{
    uint32_t n = 0u;
    const uint8_t zero_code[] = {0x02, 0x11, 0x00};
    TEST_ASSERT_EQUAL_INT(CTP_E_COBS, ctp_cobs_decode(zero_code, 3u, buf2, sizeof buf2, &n));
    const uint8_t overrun[] = {0x05, 0x11};
    TEST_ASSERT_EQUAL_INT(CTP_E_COBS, ctp_cobs_decode(overrun, 2u, buf2, sizeof buf2, &n));
    const uint8_t big[] = {0x04, 1, 2, 3};
    TEST_ASSERT_EQUAL_INT(CTP_E_OVERFLOW, ctp_cobs_decode(big, 4u, buf2, 2u, &n));
    TEST_ASSERT_EQUAL_INT(CTP_E_OVERFLOW, ctp_cobs_encode(big, 4u, buf, 3u, &n));
}

static void test_crc_vectors(void)
{
    for (size_t i = 0; i < TV_CRC32_N; i++) {
        TEST_ASSERT_EQUAL_HEX32(tv_crc32[i].crc, ctp_crc32(tv_crc32[i].in, (uint32_t)tv_crc32[i].len));
    }
}

static void test_frame_encode_vectors(void)
{
    for (size_t i = 0; i < TV_FRAMES_N; i++) {
        const tv_frame_t *v = &tv_frames[i];
        const ctp_hdr_t h = {v->h.ver, v->h.type, v->h.seq, v->h.req_id, v->h.flags, v->h.cfg_gen};
        uint32_t n = 0u;
        TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_frame_encode(buf, sizeof buf, &h, v->body, (uint32_t)v->body_len, &n));
        TEST_ASSERT_EQUAL_UINT32(v->wire_len, n);
        TEST_ASSERT_EQUAL_MEMORY(v->wire, buf, n);
        TEST_ASSERT_TRUE(n <= ctp_frame_wire_max((uint32_t)v->body_len));
    }
}

typedef struct {
    int n;
    int ids[8];
} got_t;

static bool collect(void *ctx, const ctp_hdr_t *h, const uint8_t *body, uint32_t len)
{
    (void)body;
    (void)len;
    got_t *g = (got_t *)ctx;
    if (g->n < 8) {
        g->ids[g->n] = h->req_id;
    }
    g->n++;
    return true;
}

static ctp_frame_dec_t s_dec;

static void test_stream_vectors(void)
{
    for (size_t i = 0; i < TV_STREAMS_N; i++) {
        const tv_stream_t *v = &tv_streams[i];
        for (uint32_t chunk = 1u; chunk <= 4096u; chunk *= 64u) { /* byte-at-a-time and bulk */
            ctp_frame_dec_init(&s_dec);
            got_t g = {0};
            for (size_t off = 0; off < v->len; off += chunk) {
                const size_t n = (v->len - off < chunk) ? v->len - off : chunk;
                (void)ctp_frame_dec_feed(&s_dec, &v->stream[off], (uint32_t)n, collect, &g);
            }
            TEST_ASSERT_EQUAL_INT_MESSAGE(v->frames, g.n, v->name);
            for (int k = 0; k < v->frames && k < 4; k++) {
                TEST_ASSERT_EQUAL_INT_MESSAGE(v->req_ids[k], g.ids[k], v->name);
            }
            TEST_ASSERT_EQUAL_INT_MESSAGE(v->crc_errors, (int)s_dec.st.crc_errors, v->name);
            TEST_ASSERT_EQUAL_INT_MESSAGE(v->cobs_errors, (int)s_dec.st.cobs_errors, v->name);
            TEST_ASSERT_EQUAL_INT_MESSAGE(v->oversize, (int)s_dec.st.oversize, v->name);
            TEST_ASSERT_EQUAL_INT_MESSAGE(v->shortf, (int)s_dec.st.shortf, v->name);
        }
    }
}

static const tv_msg_t *find_msg(const char *name)
{
    for (size_t i = 0; i < TV_MSGS_N; i++) {
        if (strcmp(tv_msgs[i].name, name) == 0) {
            return &tv_msgs[i];
        }
    }
    TEST_FAIL_MESSAGE(name);
    return NULL;
}

#define F(i) (m->fields[i])
#define U8(i) ((uint8_t)F(i))
#define U16(i) ((uint16_t)F(i))
#define U32(i) ((uint32_t)F(i))

static void test_request_vectors(void)
{
    for (size_t i = 0; i < TV_MSGS_N; i++) {
        const tv_msg_t *m = &tv_msgs[i];
        if (!m->is_req) {
            continue;
        }
        const uint32_t len = (uint32_t)m->len;
        switch (m->type) {
        case CTP_T_HELLO: {
            ctp_hello_t h;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_hello(m->body, len, &h));
            TEST_ASSERT_EQUAL_HEX32(U32(0), h.host_nonce);
            TEST_ASSERT_EQUAL_UINT8(U8(1), h.host_proto_ver);
            TEST_ASSERT_EQUAL_INT(CTP_E_BAD_BODY, ctp_dec_hello(m->body, 4u, &h));
            break;
        }
        case CTP_T_PING: {
            uint32_t v = 0u;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_u32(m->body, len, &v));
            TEST_ASSERT_EQUAL_UINT32(U32(0), v);
            break;
        }
        case CTP_T_SET_CHANNEL: {
            ctp_set_channel_t s;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_set_channel(m->body, len, &s));
            TEST_ASSERT_EQUAL_UINT8(U8(0), s.channel);
            TEST_ASSERT_EQUAL_UINT8(U8(1), s.bw);
            break;
        }
        case CTP_T_SET_COUNTRY: {
            ctp_country_t c;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_country(m->body, len, &c));
            TEST_ASSERT_EQUAL_INT((int)F(0), c.cc[0]);
            TEST_ASSERT_EQUAL_INT((int)F(1), c.cc[1]);
            TEST_ASSERT_EQUAL_UINT8(U8(3), c.mode);
            TEST_ASSERT_EQUAL_UINT8(U8(4), c.schan);
            TEST_ASSERT_EQUAL_UINT8(U8(5), c.nchan);
            TEST_ASSERT_EQUAL_INT(CTP_E_BAD_BODY, ctp_dec_country(m->body, 5u, &c));
            break;
        }
        case CTP_T_SET_SNAPLEN: {
            uint16_t v = 0u;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_u16(m->body, len, &v));
            TEST_ASSERT_EQUAL_UINT16(U16(0), v);
            break;
        }
        case CTP_T_SET_DRIVER_MASK: {
            ctp_driver_mask_t d;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_driver_mask(m->body, len, &d));
            TEST_ASSERT_EQUAL_HEX32(U32(0), d.type_mask);
            TEST_ASSERT_EQUAL_HEX32(U32(1), d.ctrl_mask);
            TEST_ASSERT_EQUAL_INT(CTP_E_BAD_BODY, ctp_dec_driver_mask(m->body, 7u, &d));
            break;
        }
        case CTP_T_CAPTURE:
        case CTP_T_GET_STATS:
        case CTP_T_REBOOT: {
            uint8_t v = 0u;
            TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_dec_u8(m->body, len, &v));
            TEST_ASSERT_EQUAL_UINT8(U8(0), v);
            TEST_ASSERT_EQUAL_INT(CTP_E_BAD_BODY, ctp_dec_u8(m->body, 0u, &v));
            break;
        }
        default:
            TEST_FAIL_MESSAGE(m->name);
        }
    }
}

static void test_response_vectors(void)
{
    uint8_t out[256];
    const tv_msg_t *m = find_msg("RSP_OK DeviceInfo");
    ctp_devinfo_t d;
    d.proto_ver = U8(0);
    d.fw_version = U32(1);
    d.fw_flags = U8(2);
    for (uint32_t i = 0u; i < 8u; i++) {
        d.build_id[i] = U8(3u + i);
    }
    for (uint32_t i = 0u; i < 6u; i++) {
        d.mac[i] = U8(11u + i);
    }
    d.caps = U32(17);
    d.max_frame = U16(18);
    d.max_rules = U16(19);
    d.device_time_us = (uint64_t)F(20);
    d.host_nonce = U32(21);
    d.reset_reason = U8(22);
    d.state_flags = U8(23);
    TEST_ASSERT_EQUAL_UINT32(m->len, ctp_enc_devinfo(out, sizeof out, &d));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);
    TEST_ASSERT_EQUAL_UINT32(0u, ctp_enc_devinfo(out, 10u, &d)); /* too small → 0 */

    m = find_msg("RSP_OK Pong");
    TEST_ASSERT_EQUAL_UINT32(m->len, ctp_enc_pong(out, sizeof out, U32(0), (uint64_t)F(1)));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);

    const char *cfgs[] = {"RSP_OK Config", "RSP_OK boot Config"};
    for (size_t k = 0; k < 2; k++) {
        m = find_msg(cfgs[k]);
        const ctp_config_t c = {U8(0),   U8(1),   U8(2),   U8(3), {{(char)F(4), (char)F(5), (char)F(6)}, U8(7), U8(8), U8(9)},
                                U16(10), U32(11), U32(12), U8(13), U8(14), U32(15)};
        TEST_ASSERT_EQUAL_UINT32(CTP_CONFIG_LEN, ctp_enc_config(out, sizeof out, &c));
        TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);
    }
    ctp_config_t boot;
    ctp_config_boot(&boot);
    TEST_ASSERT_EQUAL_UINT32(CTP_CONFIG_LEN, ctp_enc_config(out, sizeof out, &boot));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);

    m = find_msg("EVT_STATS");
    ctp_stats_t st = {(uint64_t)F(0), U32(1), U32(2), U32(3), U32(4), U32(5), U32(6), U32(7), U32(8),
                      U16(9), U8(10), (int8_t)F(11), U32(12)};
    TEST_ASSERT_EQUAL_UINT32(m->len, ctp_enc_stats(out, sizeof out, &st));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);

    m = find_msg("RSP_ERR");
    TEST_ASSERT_EQUAL_UINT32(m->len, ctp_enc_rsp_err(out, sizeof out, U16(0), (int32_t)F(1), "not in country table"));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);

    m = find_msg("EVT_RESET");
    TEST_ASSERT_EQUAL_UINT32(m->len, ctp_enc_reset(out, sizeof out, U8(0), U32(1), U8(2), "proto.c:123"));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, m->len);

    m = find_msg("PKT");
    ctp_pkt_meta_t p = {(uint64_t)F(0), U8(1), U8(2), (int8_t)F(3), (int8_t)F(4), U8(5), U8(6), U16(7), U16(8), U16(9)};
    TEST_ASSERT_EQUAL_UINT32(CTP_PKT_META_LEN, ctp_enc_pkt_meta(out, sizeof out, &p));
    TEST_ASSERT_EQUAL_MEMORY(m->body, out, CTP_PKT_META_LEN);
}

static void test_rd_sticky(void)
{
    const uint8_t b[3] = {1, 2, 3};
    ctp_rd_t r;
    ctp_rd_init(&r, b, 3u);
    TEST_ASSERT_EQUAL_UINT16(0x0201u, ctp_rd_u16(&r));
    TEST_ASSERT_EQUAL_UINT32(0u, ctp_rd_u32(&r)); /* past end → 0 + err */
    TEST_ASSERT_TRUE(r.err);
    TEST_ASSERT_EQUAL_UINT8(0u, ctp_rd_u8(&r)); /* sticky */
    ctp_wr_t w;
    uint8_t o[3];
    ctp_wr_init(&w, o, 3u);
    ctp_wr_u16(&w, 0xBEEFu);
    ctp_wr_u16(&w, 1u);
    TEST_ASSERT_TRUE(w.err);
    TEST_ASSERT_EQUAL_UINT32(2u, w.off);
}

static void test_rates(void)
{
    bool unk = false;
    TEST_ASSERT_EQUAL_UINT8(2u, ctp_rate_from_rxctrl(0u, 0u, &unk));
    TEST_ASSERT_FALSE(unk);
    TEST_ASSERT_EQUAL_UINT8(22u, ctp_rate_from_rxctrl(0u, 7u, &unk));
    TEST_ASSERT_EQUAL_UINT8(12u, ctp_rate_from_rxctrl(1u, 0x0Bu, &unk));
    TEST_ASSERT_EQUAL_UINT8(108u, ctp_rate_from_rxctrl(1u, 0x0Cu, &unk));
    TEST_ASSERT_EQUAL_UINT8(0u, ctp_rate_from_rxctrl(1u, 0x01u, &unk));
    TEST_ASSERT_TRUE(unk);
    (void)ctp_rate_from_rxctrl(2u, 5u, &unk);
    TEST_ASSERT_TRUE(unk);
    TEST_ASSERT_EQUAL_UINT8(7u, ctp_mcs_from_siga1(2u, 0xFF87u, &unk)); /* HT: bits 0..6 */
    TEST_ASSERT_FALSE(unk);
    TEST_ASSERT_EQUAL_UINT8(11u, ctp_mcs_from_siga1(4u, 11u << 3, &unk)); /* HE-SU: bits 3..6 */
    TEST_ASSERT_FALSE(unk);
    (void)ctp_mcs_from_siga1(3u, 0u, &unk); /* VHT */
    TEST_ASSERT_TRUE(unk);
}

void run_codec_tests(void)
{
    RUN_TEST(test_rd_sticky);
    RUN_TEST(test_cobs_vectors);
    RUN_TEST(test_cobs_errors);
    RUN_TEST(test_crc_vectors);
    RUN_TEST(test_frame_encode_vectors);
    RUN_TEST(test_stream_vectors);
    RUN_TEST(test_request_vectors);
    RUN_TEST(test_response_vectors);
    RUN_TEST(test_rates);
}
