/* Link-time API check. The hardware part of docs/10 §5 (NULL mode → 5 GHz →
 * ch 36 → BW20 → promisc, 2 s frame count) runs in the first M3 session. */
#include "esp_timer.h"

void ctk_smoke_refs(int never);

void app_main(void)
{
    ctk_smoke_refs(esp_timer_get_time() < 0); /* never true: references only */
}
