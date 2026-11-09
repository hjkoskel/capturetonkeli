module github.com/hjkoskel/capturetonkeli/examples/fpvview

go 1.22

require (
	github.com/gen2brain/raylib-go/raylib v0.60.1
	github.com/hjkoskel/capturetonkeli v0.0.0
)

require (
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/jupiterrider/ffi v0.7.0 // indirect
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842 // indirect
)

// the library from this repository
replace github.com/hjkoskel/capturetonkeli => ../..
