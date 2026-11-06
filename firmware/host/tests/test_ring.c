#include <string.h>

#include "ctp/ring.h"
#include "unity.h"

static uint8_t s_buf[1024] __attribute__((aligned(4)));
static ctp_ring_t s_r;

static void push(uint32_t len, uint8_t tag, bool expect_ok)
{
    uint8_t *p = ctp_ring_reserve(&s_r, len);
    if (!expect_ok) {
        TEST_ASSERT_NULL(p);
        return;
    }
    TEST_ASSERT_NOT_NULL(p);
    memset(p, tag, len);
    ctp_ring_commit(&s_r);
}

static void pop(uint32_t len, uint8_t tag)
{
    uint32_t l = 0u;
    const uint8_t *p = ctp_ring_peek(&s_r, &l);
    TEST_ASSERT_NOT_NULL(p);
    TEST_ASSERT_EQUAL_UINT32(len, l);
    for (uint32_t i = 0u; i < l; i++) {
        TEST_ASSERT_EQUAL_UINT8(tag, p[i]);
    }
    ctp_ring_release(&s_r);
}

static void test_ring_fifo_and_wrap(void)
{
    ctp_ring_init(&s_r, s_buf, sizeof s_buf);
    uint32_t l = 0u;
    TEST_ASSERT_NULL(ctp_ring_peek(&s_r, &l));
    for (uint32_t round = 0u; round < 50u; round++) {
        const uint32_t a = 1u + (round * 37u) % 300u;
        const uint32_t b = 1u + (round * 91u) % 200u;
        push(a, (uint8_t)round, true);
        push(b, (uint8_t)(round + 100u), true);
        pop(a, (uint8_t)round);
        pop(b, (uint8_t)(round + 100u));
        TEST_ASSERT_NULL(ctp_ring_peek(&s_r, &l));
    }
}

static void test_ring_full_rejects_whole_records(void)
{
    ctp_ring_init(&s_r, s_buf, sizeof s_buf);
    uint32_t n = 0u;
    while (ctp_ring_reserve(&s_r, 100u) != NULL) {
        ctp_ring_commit(&s_r);
        n++;
    }
    TEST_ASSERT_TRUE(n >= 9u);
    TEST_ASSERT_TRUE(ctp_ring_fill_pct(&s_r) >= 90u);
    push(2000u, 0u, false); /* never fits */
    ctp_ring_flush(&s_r);
    uint32_t l = 0u;
    TEST_ASSERT_NULL(ctp_ring_peek(&s_r, &l));
    TEST_ASSERT_EQUAL_UINT16(0u, ctp_ring_fill_pct(&s_r));
    push(500u, 7u, true);
    pop(500u, 7u);
}

void run_ring_tests(void)
{
    RUN_TEST(test_ring_fifo_and_wrap);
    RUN_TEST(test_ring_full_rejects_whole_records);
}
