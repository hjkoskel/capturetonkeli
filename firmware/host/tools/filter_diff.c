/* Differential-test helper: reads "FILTERHEX FRAMEHEX FLAGS RSSI ORIGLEN" lines
 * on stdin, prints one result per line: 1/0 = match, E = filter rejected. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "ctp/filter.h"

void ctp_assert_fail(const char *file, int line)
{
    fprintf(stderr, "CTP_ASSERT failed at %s:%d\n", file, line);
    abort();
}

static uint32_t unhex(const char *s, uint8_t *out, uint32_t cap)
{
    uint32_t n = 0u;
    if (strcmp(s, "-") == 0) {
        return 0u;
    }
    while (s[0] != '\0' && s[1] != '\0' && n < cap) {
        char b[3] = {s[0], s[1], '\0'};
        out[n++] = (uint8_t)strtoul(b, NULL, 16);
        s += 2;
    }
    return n;
}

static char line[40000];
static char fh[20000], xh[20000];
static uint8_t fb[10000], xb[10000];
static ctp_filter_t f;

int main(void)
{
    while (fgets(line, sizeof line, stdin) != NULL) {
        unsigned flags = 0u, orig = 0u;
        int rssi = 0;
        if (sscanf(line, "%19999s %19999s %u %d %u", fh, xh, &flags, &rssi, &orig) != 5) {
            return 2;
        }
        const uint32_t fl = unhex(fh, fb, sizeof fb);
        const uint32_t xl = unhex(xh, xb, sizeof xb);
        if (ctp_filter_decode(fb, fl, CTP_MAX_RULES, &f) != CTP_OK) {
            puts("E");
            continue;
        }
        const ctp_match_meta_t m = {.fcs_err = (flags & 1u) != 0u, .rssi = (int8_t)rssi, .orig_len = (uint16_t)orig};
        puts(ctp_filter_match(&f, xb, xl, &m) ? "1" : "0");
    }
    return 0;
}
