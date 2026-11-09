package video

// Small MP4 writer for OpenHD video recordings (docs/13 §13.2): H.264 ('avc1' + avcC) or
// H.265 ('hev1' + hvcC) access units in Annex B with their 90 kHz RTP timestamps → a plain
// (non-fragmented) ISO BMFF file, moov first ("fast start"), one chunk. Parameter sets stay
// in-band too, so damaged or restarted streams still decode. No B-frames assumed (OpenHD's
// encoders are low-latency): decode order = display order. Port of js/mp4.js.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sort"
)

// ---------------- SPS parsing ----------------

var errSPSShort = errors.New("video: SPS too short")

type bitReader struct {
	b   []byte
	i   int
	err error
}

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for k := 0; k < n; k++ {
		if r.i>>3 >= len(r.b) {
			r.err = errSPSShort
			return 0
		}
		v = v<<1 | uint64(r.b[r.i>>3]>>(7-(r.i&7))&1)
		r.i++
	}
	return v
}

func (r *bitReader) ue() uint64 {
	z := 0
	for r.u(1) == 0 {
		if r.err != nil {
			return 0
		}
		if z++; z > 31 {
			r.err = errors.New("video: bad Exp-Golomb code in SPS")
			return 0
		}
	}
	return (1 << z) - 1 + r.u(z)
}

func (r *bitReader) se() int64 {
	k := r.ue()
	if k&1 != 0 {
		return int64(k+1) / 2
	}
	return -int64(k) / 2
}

// SPSInfo is what the MP4 sample entry needs from the SPS.
type SPSInfo struct {
	Width, Height                int
	Profile, Compat, Level       int // H.264
	Chroma                       int
	BitDepthLuma, BitDepthChroma int
	PTL                          []byte // H.265 general profile_tier_level (12 bytes)
	MaxSubLayers                 int    // H.265
}

// ParseSPS parses an SPS NAL (with its NAL header).
func ParseSPS(codec string, sps []byte) (SPSInfo, error) {
	if codec == H264 {
		if len(sps) < 2 {
			return SPSInfo{}, errSPSShort
		}
		r := &bitReader{b: UnescapeRBSP(sps[1:])}
		profile, compat, level := int(r.u(8)), int(r.u(8)), int(r.u(8))
		r.ue()
		chroma, bdl, bdc, sep := 1, 8, 8, 0
		switch profile {
		case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
			chroma = int(r.ue())
			if chroma == 3 {
				sep = int(r.u(1))
			}
			bdl, bdc = int(r.ue())+8, int(r.ue())+8
			r.u(1)
			if r.u(1) != 0 {
				lists := 8
				if chroma == 3 {
					lists = 12
				}
				for i := 0; i < lists && r.err == nil; i++ {
					if r.u(1) == 0 {
						continue
					}
					last, next := int64(8), int64(8)
					size := 64
					if i < 6 {
						size = 16
					}
					for j := 0; j < size && r.err == nil; j++ {
						if next != 0 {
							next = (last + r.se() + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
		r.ue()
		poc := r.ue()
		if poc == 0 {
			r.ue()
		} else if poc == 1 {
			r.u(1)
			r.se()
			r.se()
			n := r.ue()
			for i := uint64(0); i < n && r.err == nil; i++ {
				r.se()
			}
		}
		r.ue()
		r.u(1)
		wMbs, hMap, frameMbsOnly := int(r.ue())+1, int(r.ue())+1, int(r.u(1))
		if frameMbsOnly == 0 {
			r.u(1)
		}
		r.u(1)
		var cl, cr, ct, cb int
		if r.u(1) != 0 {
			cl, cr, ct, cb = int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		}
		if r.err != nil {
			return SPSInfo{}, r.err
		}
		arr := chroma
		if sep != 0 {
			arr = 0
		}
		cx := 1
		if arr == 1 || arr == 2 {
			cx = 2
		}
		cy := 1
		if arr == 1 {
			cy = 2
		}
		cy *= 2 - frameMbsOnly
		return SPSInfo{Width: wMbs*16 - cx*(cl+cr), Height: (2-frameMbsOnly)*hMap*16 - cy*(ct+cb), Profile: profile, Compat: compat, Level: level,
			Chroma: chroma, BitDepthLuma: bdl, BitDepthChroma: bdc}, nil
	}
	if len(sps) < 15 {
		return SPSInfo{}, errSPSShort
	}
	rb := UnescapeRBSP(sps[2:])
	if len(rb) < 13 {
		return SPSInfo{}, errSPSShort
	}
	r := &bitReader{b: rb}
	r.u(4)
	maxSub := int(r.u(3))
	r.u(1)
	ptl := append([]byte(nil), rb[1:13]...)
	r.i += 96
	subProf, subLvl := make([]uint64, maxSub), make([]uint64, maxSub)
	for i := 0; i < maxSub; i++ {
		subProf[i], subLvl[i] = r.u(1), r.u(1)
	}
	if maxSub > 0 {
		for i := maxSub; i < 8; i++ {
			r.u(2)
		}
	}
	for i := 0; i < maxSub; i++ {
		if subProf[i] != 0 {
			r.i += 88
		}
		if subLvl[i] != 0 {
			r.i += 8
		}
	}
	r.ue()
	chroma := int(r.ue())
	if chroma == 3 {
		r.u(1)
	}
	width, height := int(r.ue()), int(r.ue())
	if r.u(1) != 0 {
		sw, sh := 1, 1
		if chroma == 1 || chroma == 2 {
			sw = 2
		}
		if chroma == 1 {
			sh = 2
		}
		l, rt, t, b := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		width -= sw * (l + rt)
		height -= sh * (t + b)
	}
	bdl, bdc := int(r.ue())+8, int(r.ue())+8
	if r.err != nil {
		return SPSInfo{}, r.err
	}
	return SPSInfo{Width: width, Height: height, Chroma: chroma, BitDepthLuma: bdl, BitDepthChroma: bdc, PTL: ptl, MaxSubLayers: maxSub + 1}, nil
}

// ---------------- boxes ----------------

func u16(v ...int) []byte {
	o := make([]byte, 2*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint16(o[2*i:], uint16(x))
	}
	return o
}

func u32(v ...uint32) []byte {
	o := make([]byte, 4*len(v))
	for i, x := range v {
		binary.BigEndian.PutUint32(o[4*i:], x)
	}
	return o
}

func box(typ string, parts ...[]byte) []byte {
	n := 8
	for _, p := range parts {
		n += len(p)
	}
	o := make([]byte, 0, n)
	o = binary.BigEndian.AppendUint32(o, uint32(n))
	o = append(o, typ...)
	for _, p := range parts {
		o = append(o, p...)
	}
	return o
}

func fullbox(typ string, version, flags uint32, parts ...[]byte) []byte {
	return box(typ, append([][]byte{u32(version<<24 | flags)}, parts...)...)
}

var matrix = u32(0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000)

func withLen16(n []byte) []byte { return append(u16(len(n)), n...) }

func avcC(sps, pps []byte, info SPSInfo) []byte {
	var ext []byte
	switch info.Profile {
	case 100, 110, 122, 144:
		ext = []byte{0xfc | byte(info.Chroma), 0xf8 | byte(info.BitDepthLuma-8), 0xf8 | byte(info.BitDepthChroma-8), 0}
	}
	return box("avcC", []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe0 | 1}, withLen16(sps), []byte{1}, withLen16(pps), ext)
}

func hvcC(vps, sps, pps []byte, info SPSInfo) []byte {
	arr := func(typ byte, n []byte) []byte { return append(append([]byte{0x80 | typ}, u16(1)...), withLen16(n)...) }
	return box("hvcC", []byte{1}, info.PTL[:11], []byte{info.PTL[11]}, u16(0xf000),
		[]byte{0xfc, 0xfc | byte(info.Chroma), 0xf8 | byte(info.BitDepthLuma-8), 0xf8 | byte(info.BitDepthChroma-8)},
		u16(0), []byte{byte((info.MaxSubLayers&7)<<3 | 3)}, []byte{3}, arr(32, vps), arr(33, sps), arr(34, pps))
}

// durations returns per-sample durations (90 kHz) from RTP timestamps; gaps and jumps fall
// back to the typical frame time.
func durations(ts []uint32) []uint32 {
	d := make([]uint32, 0, len(ts))
	for i := 0; i+1 < len(ts); i++ {
		d = append(d, ts[i+1]-ts[i])
	}
	var ok []uint32
	for _, x := range d {
		if x > 0 && x <= 90000 {
			ok = append(ok, x)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
	typical := uint32(3000)
	if len(ok) > 0 {
		typical = ok[len(ok)>>1]
	}
	for i, x := range d {
		if !(x > 0 && x <= 5*90000) {
			d[i] = typical
		}
	}
	return append(d, typical)
}

type sample struct {
	data []byte
	key  bool
	ts   uint32
}

type params struct{ vps, sps, pps []byte }

func oneFile(codec string, samples []sample, p params, info SPSInfo) ([]byte, uint64, error) {
	var mdatLen uint64
	sizes := make([]uint32, len(samples))
	ts := make([]uint32, len(samples))
	var keys []uint32
	for i, s := range samples {
		sizes[i] = uint32(len(s.data))
		mdatLen += uint64(len(s.data))
		ts[i] = s.ts
		if s.key {
			keys = append(keys, uint32(i+1))
		}
	}
	if mdatLen+8 > 0xffffffff {
		return nil, 0, errors.New("video: recording too large for one MP4 (4 GiB)")
	}
	durs := durations(ts)
	var total uint64
	for _, d := range durs {
		total += uint64(d)
	}
	ms := uint32((total + 45) / 90)
	var tt [][2]uint32 // stts runs
	for _, d := range durs {
		if n := len(tt); n > 0 && tt[n-1][1] == d {
			tt[n-1][0]++
		} else {
			tt = append(tt, [2]uint32{1, d})
		}
	}
	sampleType, cfg := "avc1", []byte(nil)
	if codec == H265 {
		sampleType, cfg = "hev1", hvcC(p.vps, p.sps, p.pps, info)
	} else {
		cfg = avcC(p.sps, p.pps, info)
	}
	entry := box(sampleType, make([]byte, 6), u16(1, 0, 0), u32(0, 0, 0), u16(info.Width, info.Height), u32(0x480000, 0x480000, 0), u16(1),
		make([]byte, 32), u16(0x18, 0xffff), cfg)
	stts := []byte{}
	for _, r := range tt {
		stts = append(stts, u32(r[0], r[1])...)
	}
	stbl := func(chunkOffset uint32) []byte {
		parts := [][]byte{fullbox("stsd", 0, 0, u32(1), entry), fullbox("stts", 0, 0, u32(uint32(len(tt))), stts)}
		if len(keys) != len(samples) {
			parts = append(parts, fullbox("stss", 0, 0, u32(append([]uint32{uint32(len(keys))}, keys...)...)))
		}
		parts = append(parts,
			fullbox("stsz", 0, 0, u32(append([]uint32{0, uint32(len(samples))}, sizes...)...)),
			fullbox("stsc", 0, 0, u32(1, 1, uint32(len(samples)), 1)),
			fullbox("stco", 0, 0, u32(1, chunkOffset)))
		return box("stbl", parts...)
	}
	w, h := uint32(info.Width), uint32(info.Height)
	moov := func(chunkOffset uint32) []byte {
		return box("moov",
			fullbox("mvhd", 0, 0, u32(0, 0, 1000, ms, 0x10000), u16(0x100, 0), u32(0, 0), matrix, u32(0, 0, 0, 0, 0, 0, 2)),
			box("trak",
				fullbox("tkhd", 0, 3, u32(0, 0, 1, 0, ms, 0, 0), u16(0, 0, 0, 0), matrix, u32(w*0x10000, h*0x10000)),
				box("mdia",
					fullbox("mdhd", 0, 0, u32(0, 0, 90000, uint32(total)), u16(0x55c4, 0)),
					fullbox("hdlr", 0, 0, u32(0), []byte("vide"), u32(0, 0, 0), []byte("VideoHandler\x00")),
					box("minf", fullbox("vmhd", 0, 1, u16(0, 0, 0, 0)), box("dinf", fullbox("dref", 0, 0, u32(1), fullbox("url ", 0, 1))), stbl(chunkOffset)))))
	}
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2"+sampleType+"mp41"))
	head := len(moov(0)) // stco is 32-bit: the size does not depend on the value
	var out bytes.Buffer
	out.Grow(len(ftyp) + head + 8 + int(mdatLen))
	out.Write(ftyp)
	out.Write(moov(uint32(len(ftyp) + head + 8)))
	out.Write(u32(uint32(mdatLen + 8)))
	out.WriteString("mdat")
	for _, s := range samples {
		out.Write(s.data)
	}
	return out.Bytes(), total, nil
}

// MP4File is one finished recording.
type MP4File struct {
	Data          []byte
	Width, Height int
	Frames        int
	Ms            int // duration
}

// WriteTo writes the file.
func (f MP4File) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(f.Data)
	return int64(n), err
}

// MP4Files turns access units (Annex B, Key, 90 kHz TS) into MP4 files. A new file starts
// when the SPS changes (e.g. the camera resolution). Units before the first key frame with
// parameter sets are skipped.
func MP4Files(codec string, aus []AU) ([]MP4File, error) {
	var files []MP4File
	var cur *struct {
		p       params
		info    SPSInfo
		samples []sample
	}
	finish := func() error {
		if cur != nil && len(cur.samples) > 0 {
			data, total, err := oneFile(codec, cur.samples, cur.p, cur.info)
			if err != nil {
				return err
			}
			files = append(files, MP4File{Data: data, Width: cur.info.Width, Height: cur.info.Height, Frames: len(cur.samples), Ms: int((total + 45) / 90)})
		}
		cur = nil
		return nil
	}
	for _, au := range aus {
		nals := SplitAnnexB(au.Data)
		var p params
		for _, n := range nals {
			if len(n) == 0 {
				continue
			}
			switch paramName(codec, nalType(codec, n)) {
			case "vps":
				p.vps = n
			case "sps":
				p.sps = n
			case "pps":
				p.pps = n
			}
		}
		complete := p.sps != nil && p.pps != nil && (codec == H264 || p.vps != nil)
		if au.Key && complete && (cur == nil || !bytes.Equal(cur.p.sps, p.sps)) {
			info, err := ParseSPS(codec, p.sps)
			if err == nil {
				if err := finish(); err != nil {
					return files, err
				}
				cur = &struct {
					p       params
					info    SPSInfo
					samples []sample
				}{p: params{vps: clone(p.vps), sps: clone(p.sps), pps: clone(p.pps)}, info: info}
			}
		}
		if cur == nil {
			continue
		}
		n := 0
		for _, x := range nals {
			n += 4 + len(x)
		}
		data := make([]byte, 0, n)
		for _, x := range nals {
			data = binary.BigEndian.AppendUint32(data, uint32(len(x)))
			data = append(data, x...)
		}
		cur.samples = append(cur.samples, sample{data: data, key: au.Key, ts: au.TS})
	}
	return files, finish()
}

func clone(b []byte) []byte {
	if b == nil {
		return nil
	}
	return append([]byte(nil), b...)
}
