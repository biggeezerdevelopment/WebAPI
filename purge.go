package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"webapi/config"
)

// purgeSpools removes each spool directory under state_dir that no enabled
// spooled output in cfg uses, such as one left by a renamed output, and
// reports each removal to stdout. Run it only while the agent is stopped or
// after it was restarted with cfg: an agent still running an older config may
// be using a directory cfg no longer names.
func purgeSpools(cfg *config.Config, stdout io.Writer) error {
	if strings.TrimSpace(cfg.StateDir) == "" {
		return errors.New("-purge-spools requires state_dir")
	}
	live := make(map[string]bool)
	for _, dir := range cfg.SpoolDirs() {
		live[filepath.Clean(dir)] = true
	}
	root := filepath.Join(cfg.StateDir, "spool")
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", filepath.Clean(root), err)
	}
	removed, total := 0, int64(0)
	for _, e := range entries {
		dir := filepath.Clean(filepath.Join(root, e.Name()))
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "output-") || live[dir] {
			continue
		}
		size := dirSize(dir)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing %s: %w", dir, err)
		}
		fmt.Fprintf(stdout, "removed %s (%d bytes)\n", dir, size)
		removed++
		total += size
	}
	switch removed {
	case 0:
		fmt.Fprintln(stdout, "no stale spool directories")
	case 1:
		fmt.Fprintf(stdout, "removed 1 stale spool directory, %d bytes\n", total)
	default:
		fmt.Fprintf(stdout, "removed %d stale spool directories, %d bytes\n", removed, total)
	}
	return nil
}

// dirSize is the total size of the regular files under dir.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
