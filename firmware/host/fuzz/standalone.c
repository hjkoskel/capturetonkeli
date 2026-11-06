/* gcc fallback for the libFuzzer targets: random + mutated-valid inputs for
 * N seconds under ASan/UBSan. Usage: fuzz_X [seconds] [seed]. */
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

#include "ctp/common.h"

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

void ctp_assert_fail(const char *file, int line)
{
    fprintf(stderr, "CTP_ASSERT failed at %s:%d\n", file, line);
    abort();
}

static uint32_t s_x;

static uint32_t rnd(void)
{
    s_x ^= s_x << 13;
    s_x ^= s_x >> 17;
    s_x ^= s_x << 5;
    return s_x;
}

int main(int argc, char **argv)
{
    const long secs = (argc > 1) ? strtol(argv[1], NULL, 0) : 10;
    s_x = (argc > 2) ? (uint32_t)strtoul(argv[2], NULL, 0) : (uint32_t)time(NULL) | 1u;
    const uint32_t seed = s_x;
    static uint8_t buf[12000];
    const time_t end = time(NULL) + secs;
    unsigned long iters = 0;
    while (time(NULL) < end) {
        for (int k = 0; k < 1000; k++) {
            const size_t n = rnd() % sizeof buf;
            const uint32_t mode = rnd() % 4u;
            for (size_t i = 0; i < n; i++) {
                uint8_t b = (uint8_t)rnd();
                if (mode == 1u && (rnd() % 16u) != 0u) {
                    b = (uint8_t)(rnd() % 4u); /* small values: plausible counts/lengths */
                } else if (mode == 2u && (rnd() % 64u) == 0u) {
                    b = 0u; /* sparse delimiters */
                }
                buf[i] = b;
            }
            (void)LLVMFuzzerTestOneInput(buf, n);
            iters++;
        }
    }
    printf("%s: %lu inputs in %ld s (seed %u), no findings\n", argv[0], iters, secs, seed);
    return 0;
}
