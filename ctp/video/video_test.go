package video

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type stream struct {
	ES  string   `json:"es"`
	RTP []string `json:"rtp"`
}

type rtpFixture map[string]stream

// GStreamer x264/x265 → rtph26xpay mtu=1440 packets and the elementary streams (same
// fixture as test/js/openhd.test.mjs).
func load(t *testing.T) rtpFixture {
	t.Helper()
	b, err := os.ReadFile("../../test/js/fixtures/ohd-rtp.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	v := rtpFixture{}
	for _, c := range []string{H264, H265} {
		var s stream
		if err := json.Unmarshal(raw[c], &s); err != nil {
			t.Fatal(err)
		}
		v[c] = s
	}
	return v
}

func hx(s string) []byte { b, _ := hex.DecodeString(s); return b }

func hexes(nals [][]byte) []string {
	var o []string
	for _, n := range nals {
		o = append(o, hex.EncodeToString(n))
	}
	return o
}

func depack(codec string, pkts []string, skip int) []AU {
	var aus []AU
	d := NewDepacketizer(codec, func(a AU) { aus = append(aus, a) })
	for i, p := range pkts {
		if i != skip {
			d.Push(hx(p), false)
		}
	}
	return aus
}

func TestRTPToAccessUnits(t *testing.T) {
	v := load(t)
	for _, c := range []string{H264, H265} {
		aus := depack(Auto, v[c].RTP, -1)
		var nals [][]byte
		for _, a := range aus {
			nals = append(nals, SplitAnnexB(a.Data)...)
		}
		if !reflect.DeepEqual(hexes(nals), hexes(SplitAnnexB(hx(v[c].ES)))) {
			t.Fatalf("%s: NAL units differ from the encoder's", c)
		}
		want := map[string]string{H264: "avc1.42c014", H265: "hev1.1.6.L60.90"}[c]
		if aus[0].Codec != c || !aus[0].Key || !aus[0].HasParams || aus[0].CodecString != want {
			t.Fatalf("%s: %+v", c, aus[0].CodecString)
		}
		// our packetizer round-trips
		pk := NewPacketizer(c)
		var back []string
		d2 := NewDepacketizer(c, func(a AU) { back = append(back, hex.EncodeToString(a.Data)) })
		for _, a := range aus {
			for _, p := range pk.Packetize(a.Data, a.TS) {
				if len(p) > 1440 {
					t.Fatal("packet over the MTU")
				}
				d2.Push(p, false)
			}
		}
		var orig []string
		for _, a := range aus {
			orig = append(orig, hex.EncodeToString(a.Data))
		}
		if !reflect.DeepEqual(back, orig) {
			t.Fatalf("%s: packetizer round trip", c)
		}
	}
	// a lost packet marks the access unit damaged
	damaged := false
	for _, a := range depack(H264, v[H264].RTP, 3) {
		damaged = damaged || a.Damaged
	}
	if !damaged {
		t.Fatal("no damaged access unit after a loss")
	}
}

func topBoxes(t *testing.T, b []byte) ([]string, map[string][2]int) {
	t.Helper()
	var order []string
	pos := map[string][2]int{}
	for i := 0; i+8 <= len(b); {
		n := int(binary.BigEndian.Uint32(b[i:]))
		typ := string(b[i+4 : i+8])
		if n < 8 {
			t.Fatalf("box %s size %d", typ, n)
		}
		order = append(order, typ)
		pos[typ] = [2]int{i, i + n}
		i += n
	}
	return order, pos
}

func TestMP4(t *testing.T) {
	v := load(t)
	tmp := t.TempDir()
	for _, c := range []string{H264, H265} {
		aus := depack(Auto, v[c].RTP, -1)
		var sps []byte
		for _, n := range SplitAnnexB(aus[0].Data) {
			if paramName(c, nalType(c, n)) == "sps" {
				sps = n
			}
		}
		info, err := ParseSPS(c, sps)
		if err != nil || info.Width != 320 || info.Height != 240 {
			t.Fatalf("%s: %+v %v", c, info, err)
		}
		files, err := MP4Files(c, aus)
		if err != nil || len(files) != 1 || files[0].Frames != len(aus) {
			t.Fatalf("%s: %d files, %v", c, len(files), err)
		}
		f := files[0].Data
		order, pos := topBoxes(t, f)
		if !reflect.DeepEqual(order, []string{"ftyp", "moov", "mdat"}) {
			t.Fatalf("%s: %v", c, order)
		}
		mdat := pos["mdat"][0] + 8
		if int(binary.BigEndian.Uint32(f[mdat:]))+4 > len(f)-mdat {
			t.Fatal("stco does not point at the first sample")
		}
		cfg := map[string]string{H264: "avcC", H265: "hvcC"}[c]
		if !bytes.Contains(f[pos["moov"][0]:pos["moov"][1]], []byte(cfg)) {
			t.Fatalf("%s: no %s", c, cfg)
		}
		// independent check with ffprobe/ffmpeg when installed
		file := filepath.Join(tmp, "t-"+c+".mp4")
		if err := os.WriteFile(file, f, 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-show_entries", "stream=codec_name,width,height,avg_frame_rate,nb_read_frames", "-of", "compact=p=0:nk=1", file).CombinedOutput()
		if err != nil {
			if _, lookErr := exec.LookPath("ffprobe"); lookErr != nil {
				continue
			}
			t.Fatalf("ffprobe: %v %s", err, out)
		}
		fields := strings.Split(strings.TrimSpace(string(out)), "|")
		wantCodec := map[string]string{H264: "h264", H265: "hevc"}[c]
		if len(fields) != 5 || fields[0] != wantCodec || fields[1] != "320" || fields[2] != "240" || fields[4] != strconv.Itoa(len(aus)) {
			t.Fatalf("%s: ffprobe %q", c, out)
		}
		rate := strings.Split(fields[3], "/")
		rn, _ := strconv.ParseFloat(rate[0], 64)
		rd, _ := strconv.ParseFloat(rate[1], 64)
		if math.Abs(rn/rd-30) > 0.1 {
			t.Fatalf("%s: frame rate %s", c, fields[3])
		}
		if out, err := exec.Command("ffmpeg", "-v", "error", "-i", file, "-f", "null", "-").CombinedOutput(); err != nil || len(out) > 0 {
			t.Fatalf("%s: ffmpeg decode: %v %s", c, err, out)
		}
	}
}

func TestMP4GapsSPSChangeNoKey(t *testing.T) {
	v := load(t)
	aus := depack(Auto, v[H264].RTP, -1)
	var lost []AU
	for i, a := range aus {
		if i != 4 && i != 5 {
			lost = append(lost, a)
		}
	}
	files, err := MP4Files(H264, lost)
	if err != nil || !bytes.Contains(files[0].Data, []byte{0, 0, 0x23, 0x28}) {
		t.Fatal("a 9000-tick (3 frame) sample duration expected")
	}
	var noKey []AU
	for _, a := range aus {
		if !a.Key {
			noKey = append(noKey, a)
		}
	}
	if files, _ := MP4Files(H264, noKey); len(files) != 0 {
		t.Fatal("a file without a key frame")
	}
	mod := append([]AU(nil), aus...)
	k2 := -1
	for i, a := range mod {
		if i > 0 && a.Key {
			k2 = i
			break
		}
	}
	var parts [][]byte
	for _, n := range SplitAnnexB(mod[k2].Data) {
		n = append([]byte(nil), n...)
		if n[0]&0x1f == 7 {
			n[len(n)-1] ^= 1
		}
		parts = append(parts, startCode, n)
	}
	mod[k2].Data = concat(parts)
	if files, _ := MP4Files(H264, mod); len(files) != 2 {
		t.Fatalf("SPS change: %d files, want 2", len(files))
	}
}

func TestRecorderMatchesMP4Files(t *testing.T) {
	v := load(t)
	dir := t.TempDir()
	for _, c := range []string{H264, H265} {
		aus := depack(Auto, v[c].RTP, -1)
		want, err := MP4Files(c, aus)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewRecorder(filepath.Join(dir, c+".mp4"))
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range aus[1:] { // starts without a key frame: skipped until the next one
			if err := r.Add(a); err != nil {
				t.Fatal(err)
			}
		}
		for _, a := range aus {
			_ = r.Add(a)
		}
		files, err := r.Close()
		if err != nil || len(files) < 1 {
			t.Fatalf("%s: %v %v", c, files, err)
		}
		got, _ := os.ReadFile(files[len(files)-1].Path)
		if _, err := os.Stat(filepath.Join(dir, c+".mp4.part")); !os.IsNotExist(err) {
			t.Fatal("spool left behind")
		}
		// the last file holds the second pass: a key frame with the same SPS does not split,
		// so compare against MP4Files of the units the recorder kept
		r2, _ := NewRecorder(filepath.Join(dir, c+"-b.mp4"))
		for _, a := range aus {
			_ = r2.Add(a)
		}
		files2, _ := r2.Close()
		got2, _ := os.ReadFile(files2[0].Path)
		if !bytes.Equal(got2, want[0].Data) || files2[0].Frames != len(aus) || files2[0].Width != 320 {
			t.Fatalf("%s: recorder output differs from MP4Files (%+v)", c, files2)
		}
		if len(got) == 0 {
			t.Fatal("empty file")
		}
		// raw Annex B
		raw := filepath.Join(dir, c+".raw")
		r3, _ := NewRecorder(raw)
		for _, a := range aus {
			_ = r3.Add(a)
		}
		if _, err := r3.Close(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(raw)
		if !bytes.HasPrefix(b, aus[0].Data) {
			t.Fatal("raw stream")
		}
	}
}
