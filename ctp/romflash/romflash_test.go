package romflash

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"net"
	"testing"
	"time"
)

func connectFake(t *testing.T, rom *FakeROM) *Flasher {
	t.Helper()
	h, d := net.Pipe()
	go func() { _ = rom.Serve(d) }()
	t.Cleanup(func() { h.Close(); d.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f, err := Connect(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestSLIPRoundTrip(t *testing.T) {
	p := []byte{0x00, 0xC0, 0xDB, 0x01, 0xC0, 0xDB}
	ch := make(chan []byte, 1)
	sr := &slipReader{frames: ch}
	sr.feed(append([]byte{0x11, 0x22}, slipEncode(p)...)) // garbage before the frame
	if got := <-ch; !bytes.Equal(got, p) {
		t.Fatalf("got % x", got)
	}
}

func TestWriteDifferentialAndVerify(t *testing.T) {
	rom := NewFakeROM()
	f := connectFake(t, rom)
	rng := rand.New(rand.NewSource(1))
	app := make([]byte, 70*1024+123)
	rng.Read(app)
	boot := make([]byte, 15000)
	rng.Read(boot)
	imgs := []Image{{"bootloader", 0x2000, boot}, {"app", 0x10000, app}}
	ctx := context.Background()
	rep, err := f.WriteImages(ctx, imgs, WriteOptions{Differential: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.SectorsWritten != 4+18 || rep.SectorsSkipped != 0 {
		t.Fatalf("first write %+v", rep)
	}
	// unchanged image: nothing written
	w0 := rom.Writes
	rep, err = f.WriteImages(ctx, imgs, WriteOptions{Differential: true})
	if err != nil || rep.SectorsWritten != 0 || rom.Writes != w0 {
		t.Fatalf("unchanged: %+v err %v writes %d", rep, err, rom.Writes-w0)
	}
	// change one byte in sector 5 of the app → exactly one sector
	app[5*SectorSize+7] ^= 1
	rep, err = f.WriteImages(ctx, imgs, WriteOptions{Differential: true})
	if err != nil || rep.SectorsWritten != 1 {
		t.Fatalf("one change: %+v err %v", rep, err)
	}
	if !bytes.Equal(rom.Flash[0x10000:0x10000+len(app)], app) {
		t.Fatal("flash content differs")
	}
}

func TestRetryOnceOnMD5Mismatch(t *testing.T) {
	rom := NewFakeROM()
	rom.CorruptN = 1
	f := connectFake(t, rom)
	img := []Image{{"app", 0x10000, bytes.Repeat([]byte{0x42}, 5000)}}
	rep, err := f.WriteImages(context.Background(), img, WriteOptions{})
	if err != nil || !rep.Parts[0].Retried || !rep.Parts[0].Verified {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestRefusesOtherChips(t *testing.T) {
	rom := NewFakeROM()
	rom.ChipID = 13
	h, d := net.Pipe()
	go func() { _ = rom.Serve(d) }()
	defer h.Close()
	_, err := Connect(context.Background(), h)
	if !errors.Is(err, ErrNotC5) {
		t.Fatalf("err %v", err)
	}
}

func TestWatchdogReset(t *testing.T) {
	rom := NewFakeROM()
	f := connectFake(t, rom)
	if err := f.HardReset(context.Background()); err != nil || rom.Resets != 1 {
		t.Fatalf("err %v resets %d", err, rom.Resets)
	}
}
