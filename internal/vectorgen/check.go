package vectorgen

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Check regenerates into a temp tree (testvectors/ + js/) and lists the files
// under dir (testvectors) and dir/../js that differ.
func Check(dir string) ([]string, error) {
	root, err := os.MkdirTemp("", "ctk-vectors-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	tmp := filepath.Join(root, "testvectors")
	if err := os.MkdirAll(filepath.Join(root, "js"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	if err := Generate(tmp); err != nil {
		return nil, err
	}
	repo := filepath.Join(dir, "..")
	var diff []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		want, _ := os.ReadFile(p)
		got, rerr := os.ReadFile(filepath.Join(repo, rel))
		if rerr != nil || !bytes.Equal(want, got) {
			diff = append(diff, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("vectorgen: %w", err)
	}
	return diff, nil
}
