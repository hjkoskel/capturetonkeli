# ctp_core: portable protocol core. Builds as an ESP-IDF component or as a
# plain static library for host tests (docs/07 §2).
set(CTP_CORE_SRCS
    src/rd.c src/cobs.c src/crc32.c src/frame.c src/wire.c src/filter.c
    src/ring.c src/config.c src/stats.c src/capture.c src/proto.c)

if(ESP_PLATFORM)
    idf_component_register(SRCS ${CTP_CORE_SRCS} INCLUDE_DIRS include PRIV_INCLUDE_DIRS src LDFRAGMENTS linker.lf)
    target_compile_options(${COMPONENT_LIB} PRIVATE -Wall -Wextra -Wconversion -Wsign-conversion -Wshadow
        -Wstrict-prototypes -Wmissing-prototypes -Wswitch-enum -Wundef -Werror)
else()
    add_library(ctp_core STATIC ${CTP_CORE_SRCS})
    target_include_directories(ctp_core PUBLIC include PRIVATE src)
endif()
