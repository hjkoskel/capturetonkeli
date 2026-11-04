# ESP-IDF adapters: the only code touching IDF APIs (docs/07 §2).
# Never touches GPIO13/14 or the USB PHY (docs/10 §3).
idf_component_register(
    SRCS usb_usj.c radio_esp.c sys_esp.c cb_esp.c synth_esp.c
    INCLUDE_DIRS include
    REQUIRES ctp_core
    PRIV_REQUIRES esp_wifi esp_event esp_timer esp_driver_usb_serial_jtag esp_app_format esp_system esp_hw_support)
# no -Wundef here: IDF headers test undefined CONFIG_* macros
target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Wshadow -Werror)
