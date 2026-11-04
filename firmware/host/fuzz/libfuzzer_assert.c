#include <stdio.h>
#include <stdlib.h>

#include "ctp/common.h"

void ctp_assert_fail(const char *file, int line)
{
    fprintf(stderr, "CTP_ASSERT failed at %s:%d\n", file, line);
    abort();
}
