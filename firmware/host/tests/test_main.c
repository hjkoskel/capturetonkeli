#include "unity.h"

void setUp(void) {}
void tearDown(void) {}

void run_codec_tests(void);
void run_filter_tests(void);
void run_ring_tests(void);
void run_proto_tests(void);

int main(void)
{
    UNITY_BEGIN();
    run_codec_tests();
    run_filter_tests();
    run_ring_tests();
    run_proto_tests();
    return UNITY_END();
}

#include <stdio.h>
#include <stdlib.h>

#include "ctp/common.h"

void ctp_assert_fail(const char *file, int line)
{
    fprintf(stderr, "CTP_ASSERT failed at %s:%d\n", file, line);
    abort();
}
