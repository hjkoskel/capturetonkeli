// Golden vectors for capturetonkeli's OpenHD receiver (docs/13 §11), produced with
// OpenHD wifibroadcast's own code: Encryption.cpp (bind phrase -> keys),
// Encryptor.cpp (session packet, auth/encrypt), FECEncoder.cpp + fec_base.cpp,
// Ieee80211Header.hpp, SimpleStream.hpp (FECDisabledEncoder), and the OpenHD
// MAVLink headers for telemetry. Frame assembly mirrors WBTxRx::tx_inject_packet
// / send_session_key (radiotap omitted: the dongle delivers the 802.11 frame);
// Gen B (2.7) mirrors WBStreamTx::prepare_and_send_packet.
//
// Build (wifibroadcast checkout at 99e53e7, OpenHD/mavlink-headers checkout, libsodium):
//   W=wifibroadcast/wifibroadcast
//   g++ -std=c++20 -O1 -w -Istub -I$W/src -I$W/src/HelperSources -I$W/src/fec -I$W/lib/fec \
//       -Imavlink-headers/mavlink/v2.0 gen_vectors.cpp $W/src/encryption/Encryption.cpp \
//       $W/src/encryption/Encryptor.cpp $W/src/fec/FECEncoder.cpp $W/lib/fec/fec_base.cpp -lsodium -o gen
//   ./gen A > test/js/fixtures/ohd-gen-a.json; ./gen B > test/js/fixtures/ohd-gen-b.json
// Only this file and stub/spdlog/spdlog.h live here; OpenHD's GPL sources are not copied into the repo.
#define private public
#include "encryption/Encryptor.h"
#undef private
#include "encryption/Encryption.h"
#include "fec/FECEncoder.h"
#include <cstring>
#include <algorithm>
#include "Ieee80211Header.hpp"
#include "SimpleStream.hpp"
#include "openhd/mavlink.h"

#include <cstdio>
#include <map>
#include <random>
#include <string>
#include <vector>

using Bytes = std::vector<uint8_t>;
static std::string hex(const uint8_t* p, size_t n) {
  static const char* d = "0123456789abcdef";
  std::string s;
  for (size_t i = 0; i < n; i++) { s += d[p[i] >> 4]; s += d[p[i] & 15]; }
  return s;
}
static std::string hex(const Bytes& b) { return hex(b.data(), b.size()); }
template <size_t N> static std::string hex(const std::array<uint8_t, N>& a) { return hex(a.data(), N); }

struct WBPacketHeader { uint8_t flags, type; uint32_t idx; uint16_t total; } __attribute__((packed));
static_assert(sizeof(WBPacketHeader) == 8);

struct Unit {  // one transmitting OpenHD instance (air or ground)
  uint8_t uid; uint64_t nonce; wb::Encryptor enc;
  std::array<uint8_t, 24> skNonce{}; std::array<uint8_t, 48> skData{};
  Unit(uint8_t u, uint64_t n, wb::Key k) : uid(u), nonce(n), enc(k) { enc.makeNewSessionKey(skNonce, skData); }
};

static std::mt19937 rng(20261106);
static bool genB = false;
static std::map<int, uint32_t> genBIdx;  // per (port, type) counter, like m_sequence_numbers_by_type per stream
static std::vector<std::string> out;

static Bytes frame(Unit& u, uint8_t port, bool encrypt, const Bytes& data) {
  Ieee80211HeaderOpenHD h{};
  h.write_ieee80211_seq_nr(0x1230);
  uint8_t rp = (uint8_t)((port << 1) | (encrypt ? 1 : 0));  // RadioPort bitfield {encrypted:1, index:7}
  h.write_radio_port_src_dst(rp);
  h.write_unique_id_src_dst(u.uid);
  const uint64_t n = u.nonce++;
  h.write_nonce(n);
  Bytes f(sizeof(h) + data.size() + 16);
  memcpy(f.data(), &h, sizeof(h));
  u.enc.set_encryption_enabled(encrypt);
  int len = u.enc.authenticate_and_encrypt(n, data.data(), (int)data.size(), f.data() + sizeof(h));
  f.resize(sizeof(h) + len);
  return f;
}
static Bytes sessionFrame(Unit& u) {
  Ieee80211HeaderOpenHD h{};
  h.write_unique_id_src_dst(u.uid);
  h.write_radio_port_src_dst(127 << 1);
  h.write_ieee80211_seq_nr(0x1230);
  h.write_nonce(u.nonce++);
  Bytes f(sizeof(h) + 72);
  memcpy(f.data(), &h, sizeof(h));
  memcpy(f.data() + sizeof(h), u.skNonce.data(), 24);
  memcpy(f.data() + sizeof(h) + 24, u.skData.data(), 48);
  return f;
}
static Bytes withGenB(int port, uint8_t type, const Bytes& d) {
  if (!genB) return d;
  WBPacketHeader w{0, type, genBIdx[port * 256 + type]++, (uint16_t)(8 + d.size())};
  Bytes r(8 + d.size());
  memcpy(r.data(), &w, 8);
  memcpy(r.data() + 8, d.data(), d.size());
  return r;
}
static void emit(const char* kind, const Bytes& f, bool drop, const std::string& extra = "") {
  char buf[64];
  snprintf(buf, sizeof buf, "\"drop\":%s", drop ? "true" : "false");
  out.push_back(std::string("{\"kind\":\"") + kind + "\"," + buf + ",\"frame\":\"" + hex(f) + "\"" + extra + "}");
}

static Bytes rtp(int seq, int len, bool marker) {  // RTP v2 header (PT 96) + random payload
  Bytes p(len);
  for (auto& b : p) b = rng();
  p[0] = 0x80; p[1] = 96 | (marker ? 0x80 : 0);
  p[2] = seq >> 8; p[3] = seq;
  return p;
}

// FEC video block: OpenHD's FECEncoder; drop[] marks fragments lost on air.
static std::vector<std::string> expectedRtp[2];
static void videoBlock(Unit& u, FECEncoder& fe, int cam, bool encrypt, int k, int m, std::vector<int> drops, int& seq) {
  std::vector<std::shared_ptr<std::vector<uint8_t>>> pk;
  for (int i = 0; i < k; i++) {
    int len = 60 + rng() % 1381;  // up to FEC_PACKET_MAX_PAYLOAD_SIZE (1441)
    pk.push_back(std::make_shared<Bytes>(rtp(seq++, len, i == k - 1)));
    expectedRtp[cam].push_back(hex(*pk.back()));
  }
  int frag = 0;
  fe.m_out_cb = [&](const uint8_t* p, int n) {
    Bytes d(p, p + n);
    bool drop = std::find(drops.begin(), drops.end(), frag) != drops.end();
    emit(cam ? "video1" : "video0", frame(u, 10 + cam, encrypt, withGenB(10 + cam, 0, d)), drop);
    frag++;
  };
  fe.encode_block(pk, m);
}

static Bytes mav(const mavlink_message_t& msg) {
  Bytes b(MAVLINK_MAX_PACKET_LEN);
  b.resize(mavlink_msg_to_send_buffer(b.data(), &msg));
  return b;
}

int main(int argc, char** argv) {
  if (sodium_init() < 0) return 1;
  genB = argc > 1 && std::string(argv[1]) == "B";
  const auto keys = wb::generate_keypair_from_bind_phrase("openhd");
  const auto custom = wb::generate_keypair_from_bind_phrase("capturetonkeli test phrase");

  Unit air(0x01, 0x00000000fffffff0ull, keys.key_1);  // nonce crosses the 32-bit boundary
  Unit gnd(0x02, 0x1234567800000000ull, keys.key_2);
  FECEncoder fe0, fe1;
  FECDisabledEncoder teleEnc, gndTeleEnc;
  int seq0 = 1000, seq1 = 5000;

  emit("session", sessionFrame(air), false);
  emit("session_gnd", sessionFrame(gnd), false);
  videoBlock(air, fe0, 0, false, 6, 2, {2, 4}, seq0);   // 2 primaries lost, 2 secondaries
  // telemetry: several MAVLink frames aggregated, injected twice (same FECDisabled seq)
  std::vector<std::string> tele;
  {
    mavlink_message_t m; Bytes agg;
    mavlink_msg_heartbeat_pack(1, 1, &m, MAV_TYPE_QUADROTOR, MAV_AUTOPILOT_ARDUPILOTMEGA, MAV_MODE_FLAG_SAFETY_ARMED | MAV_MODE_FLAG_CUSTOM_MODE_ENABLED, 5, MAV_STATE_ACTIVE);
    auto a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_gps_raw_int_pack(1, 1, &m, 123456789ull, 3, 601700000, 249400000, 35500, 120, 180, 1234, 27000, 14, 0, 0, 0, 0, 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_attitude_pack(1, 1, &m, 4242, 0.1f, -0.2f, 1.5f, 0, 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_sys_status_pack(1, 1, &m, 0, 0, 0, 250, 15800, 1234, 76, 0, 0, 0, 0, 0, 0, 0, 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_vfr_hud_pack(1, 1, &m, 12.5f, 11.0f, 270, 55, 42.0f, -0.5f);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_global_position_int_pack(1, 1, &m, 5000, 601700000, 249400000, 35500, 12000, 100, -200, 30, 27000);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_statustext_pack(1, 1, &m, MAV_SEVERITY_WARNING, "Hello from the air", 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_openhd_version_message_pack(101, 191, &m, 2, 6, 4, 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_openhd_camera_status_air_pack(101, 191, &m, 3, 1, 1, 0, 0, 8000, 5, 1280, 720, 60, 0, 0, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    mavlink_msg_openhd_core_status_pack(101, 191, &m, 63, 51, 0, 1500, 0, 0, 0, 0, 1024, 0, 7, 1, 1, 0);
    a = mav(m); agg.insert(agg.end(), a.begin(), a.end());
    tele.push_back(hex(agg));
    auto t = teleEnc.encode_packet_buffer(agg.data(), agg.size());
    emit("telemetry", frame(air, 4, true, withGenB(4, 1, t)), false);
    emit("telemetry", frame(air, 4, true, withGenB(4, 1, t)), false);  // duplicate injection
  }
  videoBlock(air, fe0, 0, false, 4, 1, {1}, seq0);      // 1 primary lost
  {
    Bytes mg{0, 0, 0, 0, 0, 20};
    uint32_t f = 5745; memcpy(mg.data() + 1, &f, 4);
    emit("mgmt", frame(air, 20, true, mg), false);       // management: no Gen B header
  }
  {  // ground uplink telemetry (encrypted with key_2) and a ground management frame
    mavlink_message_t m;
    mavlink_msg_heartbeat_pack(255, 190, &m, MAV_TYPE_GCS, MAV_AUTOPILOT_INVALID, 0, 0, 0);
    auto a = mav(m);
    auto t = gndTeleEnc.encode_packet_buffer(a.data(), a.size());
    emit("telemetry_gnd", frame(gnd, 3, true, withGenB(3, 1, t)), false);
    Bytes mg{1, 0, 0, 0, 0};
    emit("mgmt_gnd", frame(gnd, 21, true, mg), false);
  }
  videoBlock(air, fe0, 0, true, 3, 1, {}, seq0);        // "ultra secure" (encrypted) video
  videoBlock(air, fe1, 1, false, 2, 1, {0}, seq1);      // secondary camera, primary 0 lost
  // a frame with a corrupted tag (must be rejected)
  {
    FECEncoder tmp; std::vector<std::shared_ptr<Bytes>> pk{std::make_shared<Bytes>(rtp(9, 100, true))};
    Bytes last;
    tmp.m_out_cb = [&](const uint8_t* p, int n) { last.assign(p, p + n); };
    tmp.encode_block(pk, 0);
    auto f = frame(air, 10, false, withGenB(10, 0, last));
    f[30] ^= 1;
    emit("video0_badtag", f, false);
  }
  // air unit restarts: new session key, nonce and FEC block index start over
  Unit air2(0x01, 0x0000000077777700ull, keys.key_1);
  FECEncoder fe2;
  genBIdx.clear();
  emit("session", sessionFrame(air2), false);
  std::vector<std::string> before = expectedRtp[0];
  videoBlock(air2, fe2, 0, false, 5, 1, {}, seq0);

  // ---- JSON ----
  printf("{\n\"generator\":\"OpenHD wifibroadcast %s (Encryptor, FECEncoder, Ieee80211Header, FECDisabledEncoder) + OpenHD/mavlink-headers\",\n",
         genB ? "14d5fdb-style Gen B framing (WBPacketHeader)" : "99e53e7 (2.6-evo, Gen A)");
  printf("\"generation\":\"%s\",\n", genB ? "B" : "A");
  printf("\"keys\":{\"openhd\":{\"key1_pub\":\"%s\",\"key1_sec\":\"%s\",\"key2_pub\":\"%s\",\"key2_sec\":\"%s\"},\n",
         hex(keys.key_1.public_key).c_str(), hex(keys.key_1.secret_key).c_str(), hex(keys.key_2.public_key).c_str(), hex(keys.key_2.secret_key).c_str());
  printf("\"custom\":{\"phrase\":\"capturetonkeli test phrase\",\"key1_pub\":\"%s\",\"key1_sec\":\"%s\",\"key2_pub\":\"%s\",\"key2_sec\":\"%s\"}},\n",
         hex(custom.key_1.public_key).c_str(), hex(custom.key_1.secret_key).c_str(), hex(custom.key_2.public_key).c_str(), hex(custom.key_2.secret_key).c_str());
  printf("\"session_keys\":[\"%s\",\"%s\"],\"session_key_gnd\":\"%s\",\n", hex(air.enc.session_key).c_str(), hex(air2.enc.session_key).c_str(), hex(gnd.enc.session_key).c_str());
  printf("\"frames\":[\n");
  for (size_t i = 0; i < out.size(); i++) printf("%s%s\n", out[i].c_str(), i + 1 < out.size() ? "," : "");
  printf("],\n\"expect\":{\"rtp0\":[");
  for (size_t i = 0; i < expectedRtp[0].size(); i++) printf("%s\"%s\"", i ? "," : "", expectedRtp[0][i].c_str());
  printf("],\"rtp0_before_restart\":%zu,\"rtp1\":[", before.size());
  for (size_t i = 0; i < expectedRtp[1].size(); i++) printf("%s\"%s\"", i ? "," : "", expectedRtp[1][i].c_str());
  printf("],\"telemetry\":[\"%s\"],\"mgmt\":{\"freq\":5745,\"bw\":20}}\n}\n", tele[0].c_str());
}
