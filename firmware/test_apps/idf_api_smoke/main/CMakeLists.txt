set(API ${CMAKE_CURRENT_LIST_DIR}/../../../idf-api.txt)
set(GEN ${CMAKE_CURRENT_BINARY_DIR}/smoke_symbols.c)
idf_component_register(SRCS smoke_main.c ${GEN}
    PRIV_REQUIRES esp_wifi esp_event esp_timer esp_driver_usb_serial_jtag esp_app_format esp_system esp_hw_support esp_rom esp_phy)
if(NOT CMAKE_BUILD_EARLY_EXPANSION)
    add_custom_command(OUTPUT ${GEN}
        COMMAND ${PYTHON} ${CMAKE_CURRENT_LIST_DIR}/../gen_smoke.py gen ${API} ${GEN}
        DEPENDS ${API} ${CMAKE_CURRENT_LIST_DIR}/../gen_smoke.py)
endif()
