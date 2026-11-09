package video

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RecordedFile is one file written by a Recorder.
type RecordedFile struct {
	Path          string
	Width, Height int
	Frames        int
	Ms            int
}

// Recorder writes access units to disk as they arrive (docs/13 §13.2):
//
//   - path ending in .mp4: MP4 like MP4Files (moov first, so samples are spooled to
//     path + ".part" and the file is assembled on a key frame with a new SPS and on Close;
//     a new SPS or 3.9 GiB of video continues in name-2.mp4, name-3.mp4, ...);
//   - any other path (.h264, .h265, .hevc): the raw Annex B stream from the first key frame.
//
// Memory use does not grow with the recording length. Not safe for concurrent use.
type Recorder struct {
	path  string
	mp4   bool
	codec string

	raw   *os.File
	spool *os.File
	cur   *recFile
	n     int
	files []RecordedFile
}

type recFile struct {
	p       params
	info    SPSInfo
	samples []sample // size, key, ts only; the data is in the spool
	bytes   uint64
}

const maxMdat = 3900 << 20

// NewRecorder starts a recording at path (see Recorder).
func NewRecorder(path string) (*Recorder, error) {
	r := &Recorder{path: path, mp4: strings.EqualFold(filepath.Ext(path), ".mp4")}
	var err error
	if r.mp4 {
		r.spool, err = os.Create(path + ".part")
	} else {
		r.raw, err = os.Create(path)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Add records one access unit. Units before the first key frame with parameter sets are
// skipped (the file would not decode).
func (r *Recorder) Add(au AU) error {
	if r.codec == "" {
		if !au.Key || !au.HasParams {
			return nil
		}
		r.codec = au.Codec
	}
	if au.Codec != r.codec {
		return fmt.Errorf("video: codec changed from %s to %s during the recording", r.codec, au.Codec)
	}
	if !r.mp4 {
		_, err := r.raw.Write(au.Data)
		return err
	}
	nals := SplitAnnexB(au.Data)
	var p params
	for _, n := range nals {
		if len(n) == 0 {
			continue
		}
		switch paramName(r.codec, nalType(r.codec, n)) {
		case "vps":
			p.vps = n
		case "sps":
			p.sps = n
		case "pps":
			p.pps = n
		}
	}
	complete := p.sps != nil && p.pps != nil && (r.codec == H264 || p.vps != nil)
	if au.Key && complete && (r.cur == nil || !bytes.Equal(r.cur.p.sps, p.sps) || r.cur.bytes > maxMdat) {
		if info, err := ParseSPS(r.codec, p.sps); err == nil {
			if err := r.finish(); err != nil {
				return err
			}
			r.cur = &recFile{p: params{vps: clone(p.vps), sps: clone(p.sps), pps: clone(p.pps)}, info: info}
		}
	}
	if r.cur == nil {
		return nil
	}
	var buf []byte
	for _, x := range nals {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(x)))
		buf = append(buf, x...)
	}
	if _, err := r.spool.Write(buf); err != nil {
		return err
	}
	r.cur.samples = append(r.cur.samples, sample{size: len(buf), key: au.Key, ts: au.TS})
	r.cur.bytes += uint64(len(buf))
	return nil
}

func (r *Recorder) nextPath() string {
	r.n++
	if r.n == 1 {
		return r.path
	}
	ext := filepath.Ext(r.path)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(r.path, ext), r.n, ext)
}

// finish assembles the current MP4 from the spool.
func (r *Recorder) finish() error {
	c := r.cur
	r.cur = nil
	if c == nil || len(c.samples) == 0 {
		return nil
	}
	head, total, err := mp4Head(r.codec, c.samples, c.p, c.info)
	if err != nil {
		return err
	}
	path := r.nextPath()
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err = out.Write(head); err == nil {
		_, err = io.Copy(out, io.NewSectionReader(r.spool, 0, int64(c.bytes)))
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	r.files = append(r.files, RecordedFile{Path: path, Width: c.info.Width, Height: c.info.Height, Frames: len(c.samples), Ms: int((total + 45) / 90)})
	if err := r.spool.Truncate(0); err != nil {
		return err
	}
	_, err = r.spool.Seek(0, io.SeekStart)
	return err
}

// Files returns the files finished so far.
func (r *Recorder) Files() []RecordedFile { return r.files }

// Close finishes the recording and returns every file written (none when no key frame
// arrived).
func (r *Recorder) Close() ([]RecordedFile, error) {
	if !r.mp4 {
		err := r.raw.Close()
		if r.codec == "" {
			_ = os.Remove(r.path)
			return nil, err
		}
		return []RecordedFile{{Path: r.path}}, err
	}
	err := r.finish()
	name := r.spool.Name()
	if cerr := r.spool.Close(); err == nil {
		err = cerr
	}
	_ = os.Remove(name)
	return r.files, err
}
