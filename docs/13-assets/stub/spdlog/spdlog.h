#pragma once
#include <format>
#include <memory>
#include <string>
namespace fmt { using std::format; }
namespace spdlog { struct logger {
  template <class... A> void warn(A&&...) {} template <class... A> void info(A&&...) {}
  template <class... A> void debug(A&&...) {} template <class... A> void error(A&&...) {}
  template <class... A> void trace(A&&...) {} template <class... A> void critical(A&&...) {} }; }
namespace wifibroadcast::log { inline std::shared_ptr<spdlog::logger> get_default() { return std::make_shared<spdlog::logger>(); }
inline std::shared_ptr<spdlog::logger> create_or_get(const std::string&) { return get_default(); } }
