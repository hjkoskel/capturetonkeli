package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	rl "github.com/gen2brain/raylib-go/raylib"
)

// picture is one decoded video frame (RGBA, w*h*4 bytes).
type picture struct {
	w, h int
	pix  []byte
	seq  uint64
}

// shared is what the pipeline hands to the window (latest picture, overlay text).
type shared struct {
	mu     sync.Mutex
	pic    *picture
	seq    uint64
	status []string
	source string
	note   string
	shown  int // pictures displayed in the last second (window side)
}

func newShared() *shared { return &shared{} }

func (s *shared) setPicture(w, h int, pix []byte) {
	s.mu.Lock()
	s.seq++
	s.pic = &picture{w: w, h: h, pix: pix, seq: s.seq}
	s.mu.Unlock()
}

func (s *shared) setStatus(l []string) { s.mu.Lock(); s.status = l; s.mu.Unlock() }
func (s *shared) setSource(t string)   { s.mu.Lock(); s.source = t; s.mu.Unlock() }
func (s *shared) setNote(t string)     { s.mu.Lock(); s.note = t; s.mu.Unlock() }

func (s *shared) snapshot() (*picture, []string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pic, s.status, s.source, s.note
}

// window runs the raylib loop on the main thread until the window closes or ctx ends.
func window(ctx context.Context, cfg *config, sh *shared) error {
	rl.SetConfigFlags(rl.FlagWindowResizable | rl.FlagVsyncHint | rl.FlagMsaa4xHint)
	rl.SetTraceLogLevel(rl.LogWarning)
	rl.InitWindow(int32(cfg.width), int32(cfg.height), "fpvview – "+map[string]string{"openhd": "OpenHD", "hx": "hx-esp32-cam-fpv"}[cfg.system])
	defer rl.CloseWindow()
	rl.SetExitKey(rl.KeyEscape)
	rl.SetTargetFPS(120) // VSync limits it; a high cap keeps the added latency below one refresh

	fs := &fullscreen{w: cfg.width, h: cfg.height}
	if cfg.fullscreen {
		fs.toggle()
	}
	var tex rl.Texture2D
	var texW, texH int
	var lastSeq uint64
	showOSD := cfg.osd
	frames, fps, fpsT := 0, 0, time.Now()

	for !rl.WindowShouldClose() {
		select {
		case <-ctx.Done():
			return finish(cfg)
		default:
		}
		alt := rl.IsKeyDown(rl.KeyLeftAlt) || rl.IsKeyDown(rl.KeyRightAlt)
		if (alt && rl.IsKeyPressed(rl.KeyEnter)) || rl.IsKeyPressed(rl.KeyF11) {
			fs.toggle()
		}
		if rl.IsKeyPressed(rl.KeyO) {
			showOSD = !showOSD
		}
		if rl.IsKeyPressed(rl.KeyQ) {
			break
		}
		if rl.IsKeyPressed(rl.KeyS) {
			name := time.Now().Format("fpvview-20060102-150405.png")
			rl.TakeScreenshot(name)
			sh.setNote("saved " + name)
		}

		pic, status, source, note := sh.snapshot()
		if pic != nil && pic.seq != lastSeq {
			if pic.w != texW || pic.h != texH {
				if texW != 0 {
					rl.UnloadTexture(tex)
				}
				img := rl.GenImageColor(pic.w, pic.h, rl.Black)
				tex = rl.LoadTextureFromImage(img)
				rl.UnloadImage(img)
				rl.SetTextureFilter(tex, rl.FilterBilinear)
				texW, texH = pic.w, pic.h
			}
			rl.UpdateTexture(tex, pic.pix)
			lastSeq = pic.seq
			frames++
		}
		if time.Since(fpsT) >= time.Second {
			fps, frames, fpsT = frames, 0, time.Now()
		}

		rl.BeginDrawing()
		rl.ClearBackground(rl.Black)
		sw, shh := float32(rl.GetRenderWidth()), float32(rl.GetRenderHeight())
		if texW != 0 {
			drawFitted(tex, texW, texH, sw, shh, cfg.flip)
		} else {
			msg := "waiting for video..."
			if note != "" {
				msg = note
			}
			tw := rl.MeasureText(msg, 28)
			rl.DrawText(msg, int32(sw/2)-tw/2, int32(shh/2)-14, 28, rl.Gray)
		}
		if showOSD {
			lines := append([]string{fmt.Sprintf("%s · shown %d fps", source, fps)}, status...)
			if note != "" && texW != 0 {
				lines = append(lines, note)
			}
			drawOverlay(lines)
		}
		rl.EndDrawing()
	}
	return finish(cfg)
}

func finish(cfg *config) error {
	if cfg.screenshot != "" {
		rl.TakeScreenshot(cfg.screenshot)
	}
	return nil
}

// drawFitted scales the picture into the window keeping its aspect ratio; flips are done by
// the GPU through negative source sizes (no pixel copies).
func drawFitted(tex rl.Texture2D, w, h int, sw, sh float32, flip string) {
	scale := min(sw/float32(w), sh/float32(h))
	dw, dh := float32(w)*scale, float32(h)*scale
	src := rl.NewRectangle(0, 0, float32(w), float32(h))
	switch flip {
	case "mirror":
		src.Width = -src.Width
	case "vflip":
		src.Height = -src.Height
	case "rot180":
		src.Width, src.Height = -src.Width, -src.Height
	}
	dst := rl.NewRectangle((sw-dw)/2, (sh-dh)/2, dw, dh)
	rl.DrawTexturePro(tex, src, dst, rl.NewVector2(0, 0), 0, rl.White)
}

func drawOverlay(lines []string) {
	const size, pad = 18, 6
	var w int32
	for _, l := range lines {
		w = max(w, rl.MeasureText(l, size))
	}
	h := int32(len(lines))*(size+4) + 2*pad
	rl.DrawRectangle(8, 8, w+2*pad, h, rl.NewColor(0, 0, 0, 150))
	for i, l := range lines {
		col := rl.RayWhite
		if strings.HasPrefix(l, "REC") {
			col = rl.NewColor(255, 90, 90, 255)
		}
		rl.DrawText(l, 8+pad, 8+pad+int32(i)*(size+4), size, col)
	}
}

// fullscreen toggles between a window and full screen at the monitor's resolution
// (the window is resized first, so the monitor mode does not change).
type fullscreen struct {
	on   bool
	w, h int // window size to go back to
}

func (f *fullscreen) toggle() {
	if !f.on {
		f.w, f.h = rl.GetScreenWidth(), rl.GetScreenHeight()
		m := rl.GetCurrentMonitor()
		rl.SetWindowSize(rl.GetMonitorWidth(m), rl.GetMonitorHeight(m))
		rl.ToggleFullscreen()
		f.on = true
		return
	}
	rl.ToggleFullscreen()
	rl.SetWindowSize(f.w, f.h)
	f.on = false
}
