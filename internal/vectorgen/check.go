package vectorgen

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Check regenerates into a temp dir and lists files in dir that differ.
func Check(dir string) ([]string, error) {
	tmp, err := os.MkdirTemp("", "ctk-vectors-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := Generate(tmp); err != nil {
		return nil, err
	}
	var diff []string
	err = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		want, _ := os.ReadFile(p)
		got, rerr := os.ReadFile(filepath.Join(dir, rel))
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
