# IDF API smoke build (docs/10 §5): compiles and links every IDF symbol listed
# in firmware/idf-api.txt, with the firmware's sdkconfig.defaults.
cmake_minimum_required(VERSION 3.22)
set(SDKCONFIG_DEFAULTS "${CMAKE_CURRENT_LIST_DIR}/../../sdkconfig.defaults")
include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(idf_api_smoke)
