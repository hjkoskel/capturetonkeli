# capturetonkeli firmware (ESP-IDF, pinned in idf-version.txt; docs/07, docs/10)
cmake_minimum_required(VERSION 3.22)

# Firmware version from the git tag vMAJOR.MINOR.PATCH (docs/07 M7). Untagged
# builds are "0.1.0-dev"; a modified tree appends "-dirty".
execute_process(COMMAND git describe --tags --match "v[0-9]*" --dirty
                WORKING_DIRECTORY ${CMAKE_CURRENT_LIST_DIR}
                OUTPUT_VARIABLE CTK_DESCRIBE OUTPUT_STRIP_TRAILING_WHITESPACE ERROR_QUIET)
if(DEFINED ENV{CTK_RELEASE_VERSION})
    # make release VERSION=X.Y.Z (docs/11 §4): strict semver, no dev flag
    if(NOT "$ENV{CTK_RELEASE_VERSION}" MATCHES "^[0-9]+\\.[0-9]+\\.[0-9]+$")
        message(FATAL_ERROR "CTK_RELEASE_VERSION must be MAJOR.MINOR.PATCH, got '$ENV{CTK_RELEASE_VERSION}'")
    endif()
    set(PROJECT_VER "$ENV{CTK_RELEASE_VERSION}")
elseif(CTK_DESCRIBE MATCHES "^v([0-9]+\\.[0-9]+\\.[0-9]+)(-dirty)?$")
    set(PROJECT_VER "${CMAKE_MATCH_1}${CMAKE_MATCH_2}")
elseif(CTK_DESCRIBE MATCHES "^v([0-9]+\\.[0-9]+\\.[0-9]+)-.*")
    set(PROJECT_VER "${CMAKE_MATCH_1}-dev")
    if(CTK_DESCRIBE MATCHES "-dirty$")
        set(PROJECT_VER "${PROJECT_VER}-dirty")
    endif()
else()
    set(PROJECT_VER "0.1.0-dev")
endif()

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(ctk)
