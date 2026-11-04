/* C matcher vs Go reference on testvectors/filter_*.json (via tv_filter.h). */
#include <stdio.h>

#include "ctp/filter.h"
#include "tv_filter.h"
#include "unity.h"

static ctp_filter_t s_f;

static void test_encode_vectors_decode(void)
{
    for (size_t i = 0; i < TV_FLT_ENC_N; i++) {
        const tv_flt_enc_t *v = &tv_flt_enc[i];
        TEST_ASSERT_EQUAL_INT_MESSAGE(CTP_OK, ctp_filter_decode(v->body, (uint32_t)v->len, CTP_MAX_RULES, &s_f), v->name);
        TEST_ASSERT_EQUAL_HEX32_MESSAGE(v->crc, s_f.crc32, v->name);
        TEST_ASSERT_EQUAL_UINT8_MESSAGE(v->body[0], s_f.n_rules, v->name);
    }
}

static void test_match_vectors(void)
{
    for (size_t i = 0; i < TV_FLT_MATCH_N; i++) {
        const tv_flt_match_t *v = &tv_flt_match[i];
        const tv_flt_enc_t *e = &tv_flt_enc[v->filter];
        const tv_flt_frame_t *fr = &tv_flt_frames[v->frame];
        TEST_ASSERT_EQUAL_INT(CTP_OK, ctp_filter_decode(e->body, (uint32_t)e->len, CTP_MAX_RULES, &s_f));
        const ctp_match_meta_t m = {.fcs_err = (v->flags & CTP_PKT_FCS_ERR) != 0u, .rssi = v->rssi, .orig_len = v->orig_len};
        const bool got = ctp_filter_match(&s_f, fr->data, (uint32_t)fr->len, &m);
        if (got != v->expect) {
            char msg[96];
            (void)snprintf(msg, sizeof msg, "case %u: filter '%s' frame %d flags %u", (unsigned)i, e->name, v->frame, v->flags);
            TEST_FAIL_MESSAGE(msg);
        }
    }
}

static void test_invalid_vectors(void)
{
    for (size_t i = 0; i < TV_FLT_BAD_N; i++) {
        TEST_ASSERT_EQUAL_INT_MESSAGE(CTP_E_FILTER_INVALID,
                                      ctp_filter_decode(tv_flt_bad[i].body, (uint32_t)tv_flt_bad[i].len, CTP_MAX_RULES, &s_f),
                                      tv_flt_bad[i].name);
    }
}

static void test_max_rules_limit(void)
{
    const tv_flt_enc_t *v = &tv_flt_enc[TV_FLT_ENC_N - 1u]; /* "max rules": 16 rules */
    TEST_ASSERT_EQUAL_UINT32(2328u, v->len);
    TEST_ASSERT_EQUAL_INT(CTP_E_FILTER_INVALID, ctp_filter_decode(v->body, (uint32_t)v->len, 15u, &s_f));
}

static void test_empty(void)
{
    ctp_filter_empty(&s_f);
    TEST_ASSERT_EQUAL_HEX32(CTP_EMPTY_FILTER_CRC, s_f.crc32);
    const ctp_match_meta_t m = {.fcs_err = true, .rssi = -99, .orig_len = 0u};
    TEST_ASSERT_TRUE(ctp_filter_match(&s_f, NULL, 0u, &m));
}

void run_filter_tests(void)
{
    RUN_TEST(test_encode_vectors_decode);
    RUN_TEST(test_match_vectors);
    RUN_TEST(test_invalid_vectors);
    RUN_TEST(test_max_rules_limit);
    RUN_TEST(test_empty);
}
