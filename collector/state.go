package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const stateVersion = 1

// checkpoint is the newest event delivered to one output: its timestamp in
// milliseconds and the IDs already delivered at exactly that timestamp.
type checkpoint struct {
	TimestampMS int64
	IDs         []string
}

type stateFile struct {
	Version int                  `json:"version"`
	Source  string               `json:"source"`
	Outputs map[string]diskPoint `json:"outputs"`
}

// diskPoint uses a pointer so that a missing timestamp is detected as
// corruption rather than read as 1970.
type diskPoint struct {
	TimestampMS *int64   `json:"timestamp_ms"`
	IDs         []string `json:"ids"`
}

// statePath hashes the source name, which may hold characters or reserved
// names that are not valid in a file name on every platform.
func statePath(dir, source string) string {
	sum := sha256.Sum256([]byte(source))
	return filepath.Join(dir, "collector-"+hex.EncodeToString(sum[:16])+".json")
}

// loadState returns the saved checkpoints. Only a missing file means no state;
// anything unreadable is an error so that history is never silently replayed.
func loadState(path, source string, now time.Time) (map[string]checkpoint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]checkpoint{}, nil
	}
	if err != nil {
		return nil, errors.New("state file cannot be read")
	}

	var file stateFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, errors.New("state file is corrupt: invalid JSON")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("state file is corrupt: trailing data")
	}
	if file.Version != stateVersion {
		return nil, fmt.Errorf("state file is corrupt: unsupported version %d", file.Version)
	}
	if file.Source != source {
		return nil, errors.New("state file is corrupt: it belongs to another source")
	}
	if file.Outputs == nil {
		return nil, errors.New("state file is corrupt: outputs missing")
	}

	state := make(map[string]checkpoint, len(file.Outputs))
	for name, point := range file.Outputs {
		switch {
		case point.TimestampMS == nil:
			return nil, fmt.Errorf("state file is corrupt: output %q has no timestamp", name)
		case *point.TimestampMS < 0:
			return nil, fmt.Errorf("state file is corrupt: output %q has a negative checkpoint", name)
		case *point.TimestampMS > now.UnixMilli():
			return nil, fmt.Errorf("state file is corrupt: output %q has a checkpoint in the future", name)
		}
		for _, id := range point.IDs {
			if id == "" {
				return nil, fmt.Errorf("state file is corrupt: output %q has an empty event ID", name)
			}
		}
		state[name] = checkpoint{TimestampMS: *point.TimestampMS, IDs: point.IDs}
	}
	return state, nil
}

// saveState replaces the file atomically: a crash leaves the old or new state,
// never a partial one.
func saveState(path, source string, state map[string]checkpoint) error {
	file := stateFile{Version: stateVersion, Source: source, Outputs: make(map[string]diskPoint, len(state))}
	for name, point := range state {
		ts := point.TimestampMS
		file.Outputs[name] = diskPoint{TimestampMS: &ts, IDs: point.IDs}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return errors.New("error encoding state")
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return errors.New("error creating temporary state file")
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		return errors.New("error writing temporary state file")
	}
	if err := replaceFile(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return errors.New("error replacing state file")
	}
	syncDir(dir)
	return nil
}

// replaceFile renames over the target. On Windows a scanner or indexer can hold
// the target open briefly, so sharing failures are retried.
func replaceFile(from, to string) error {
	err := os.Rename(from, to)
	if runtime.GOOS != "windows" {
		return err
	}
	for attempt := 0; err != nil && attempt < 5; attempt++ {
		time.Sleep(20 * time.Millisecond)
		err = os.Rename(from, to)
	}
	return err
}

// syncDir makes the rename durable. Windows cannot sync a directory handle.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
