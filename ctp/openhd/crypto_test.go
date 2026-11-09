package openhd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

func fixture(t testing.TB, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../test/js/fixtures/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func hx(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func toHex(b []byte) string { return hex.EncodeToString(b) }

type sodium struct {
	Poly1305  []struct{ Key, Msg, Tag string }
	Hsalsa20  []struct{ Key, In, Out string }
	Hchacha20 []struct{ Key, In, Out string }
	Chacha20  []struct {
		Key, Nonce string
		Ic         uint64
		Msg, Out   string
	}
	Secretbox []struct{ Key, Nonce, Msg, Box string }
	Aead      []struct {
		Key     string
		Nonce   json.Number
		Msg, Ct string
	}
	X25519 []struct{ Scalar, U, Out, Base string }
	Box    []struct {
		Seed, Pk, Sk string
		PeerPk       string `json:"peer_pk"`
		Beforenm     string
	}
	Blake2b []struct {
		Msg    string
		Outlen int
		Out    string
	}
	Argon2id []struct {
		Pw, Salt string
		T        int
		MKib     int `json:"m_kib"`
		Out      string
	}
}

// Same libsodium vectors as test/js/openhd.test.mjs.
func TestCryptoMatchesLibsodium(t *testing.T) {
	var S sodium
	fixture(t, "ohd-sodium.json", &S)
	for _, v := range S.Poly1305 {
		if tag := Poly1305(hx(v.Key), hx(v.Msg)); toHex(tag[:]) != v.Tag {
			t.Fatalf("poly1305 len %d", len(v.Msg)/2)
		}
		// split input gives the same tag
		m := hx(v.Msg)
		if tag := Poly1305(hx(v.Key), m[:len(m)/3], m[len(m)/3:]); toHex(tag[:]) != v.Tag {
			t.Fatalf("poly1305 split len %d", len(m))
		}
	}
	for _, v := range S.Hsalsa20 {
		if o := HSalsa20(hx(v.Key), hx(v.In)); toHex(o[:]) != v.Out {
			t.Fatal("hsalsa20")
		}
	}
	for _, v := range S.Hchacha20 {
		if o := HChaCha20(hx(v.Key), hx(v.In)); toHex(o[:]) != v.Out {
			t.Fatal("hchacha20")
		}
	}
	for _, v := range S.Chacha20 {
		if toHex(ChaCha20XOR(hx(v.Key), hx(v.Nonce), hx(v.Msg), v.Ic)) != v.Out {
			t.Fatalf("chacha20 ic %d", v.Ic)
		}
	}
	for _, v := range S.Secretbox {
		if toHex(SecretboxSeal(hx(v.Msg), hx(v.Nonce), hx(v.Key))) != v.Box {
			t.Fatal("secretbox seal")
		}
		m, ok := SecretboxOpen(hx(v.Box), hx(v.Nonce), hx(v.Key))
		if !ok || toHex(m) != v.Msg {
			t.Fatal("secretbox open")
		}
		bad := hx(v.Box)
		bad[0] ^= 1
		if _, ok := SecretboxOpen(bad, hx(v.Nonce), hx(v.Key)); ok {
			t.Fatal("secretbox opened a bad box")
		}
	}
	for _, v := range S.Aead {
		n, err := strconv.ParseUint(v.Nonce.String(), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if toHex(AEADSeal(hx(v.Key), n, hx(v.Msg))) != v.Ct {
			t.Fatal("aead seal")
		}
		if m, ok := AEADOpen(hx(v.Key), n, hx(v.Ct)); !ok || toHex(m) != v.Msg {
			t.Fatal("aead open")
		}
		if _, ok := AEADOpen(hx(v.Key), n+1, hx(v.Ct)); ok {
			t.Fatal("aead opened with the wrong nonce")
		}
	}
}

func TestKeysMatchLibsodiumAndOpenHD(t *testing.T) {
	var S sodium
	fixture(t, "ohd-sodium.json", &S)
	for _, v := range S.X25519 {
		if toHex(X25519(hx(v.Scalar), hx(v.U))) != v.Out || toHex(X25519(hx(v.Scalar), nil)) != v.Base {
			t.Fatal("x25519")
		}
	}
	for _, v := range S.Box {
		pk, sk := SeedKeypair(hx(v.Seed))
		if toHex(pk) != v.Pk || toHex(sk) != v.Sk || toHex(Beforenm(hx(v.PeerPk), sk)) != v.Beforenm {
			t.Fatal("seed keypair / beforenm")
		}
	}
	for _, v := range S.Blake2b {
		if toHex(Blake2b(v.Outlen, hx(v.Msg))) != v.Out {
			t.Fatalf("blake2b %d/%d", len(v.Msg)/2, v.Outlen)
		}
	}
	for _, v := range S.Argon2id {
		if toHex(Argon2id(hx(v.Pw), hx(v.Salt), v.T, v.MKib, len(v.Out)/2, nil)) != v.Out {
			t.Fatalf("argon2id t=%d m=%d", v.T, v.MKib)
		}
	}
	var g genFixture
	fixture(t, "ohd-gen-a.json", &g)
	k, err := KeysFromKeyFile(hx(g.Keys.OpenHD.Key1Pub + g.Keys.OpenHD.Key1Sec + g.Keys.OpenHD.Key2Pub + g.Keys.OpenHD.Key2Sec))
	if err != nil {
		t.Fatal(err)
	}
	if d := DefaultKeys(); !bytes.Equal(k.Air, d.Air) || !bytes.Equal(k.Gnd, d.Gnd) {
		t.Fatal("txrx.key of the default phrase != DefaultKeys")
	}
	if _, err := KeysFromKeyFile(make([]byte, 127)); err == nil {
		t.Fatal("short key file accepted")
	}
}

// Full Argon2id (2 × 64 MiB): a few seconds; skipped with -short.
func TestCustomBindPhrase(t *testing.T) {
	if testing.Short() {
		t.Skip("full Argon2id")
	}
	var g genFixture
	fixture(t, "ohd-gen-a.json", &g)
	c := g.Keys.Custom
	want, _ := KeysFromKeyFile(hx(c.Key1Pub + c.Key1Sec + c.Key2Pub + c.Key2Sec))
	last := 0.0
	got := KeysFromPhrase(c.Phrase, func(p float64) {
		if p < last {
			t.Error("progress went backwards")
		}
		last = p
	})
	if !bytes.Equal(got.Air, want.Air) || !bytes.Equal(got.Gnd, want.Gnd) || last != 1 {
		t.Fatalf("keys %x/%x, progress %v", got.Air, got.Gnd, last)
	}
}

type keyFile struct {
	Phrase  string
	Key1Pub string `json:"key1_pub"`
	Key1Sec string `json:"key1_sec"`
	Key2Pub string `json:"key2_pub"`
	Key2Sec string `json:"key2_sec"`
}

type genFixture struct {
	Keys struct {
		OpenHD keyFile `json:"openhd"`
		Custom keyFile `json:"custom"`
	}
	Frames []struct {
		Kind  string
		Drop  bool
		Frame string
	}
	Expect struct {
		RTP0      []string `json:"rtp0"`
		RTP1      []string `json:"rtp1"`
		Telemetry []string
		Mgmt      struct{ Freq, Bw int }
	}
}

func BenchmarkAuthOpen1400(b *testing.B) {
	key := bytes.Repeat([]byte{7}, 32)
	p := AuthSeal(key, 42, make([]byte, 1400))
	b.SetBytes(int64(len(p)))
	for i := 0; i < b.N; i++ {
		if _, ok := AuthOpen(key, 42, p); !ok {
			b.Fatal("auth")
		}
	}
}
