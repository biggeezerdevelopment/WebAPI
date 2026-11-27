package spool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// position is a frame boundary: a segment sequence number and byte offset.
type position struct {
	Seq uint64
	Off int64
}

// ackFile uses pointers so missing fields are detected as corruption.
type ackFile struct {
	Segment *uint64 `json:"segment"`
	Offset  *int64  `json:"offset"`
}

// readAck returns the saved read position. Only a missing file means none;
// anything unreadable is an error so records are never silently replayed or
// skipped.
func readAck(path string) (position, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return position{}, false, nil
	}
	if err != nil {
		return position{}, false, errors.New("spool ack file cannot be read")
	}
	var a ackFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil || a.Segment == nil || a.Offset == nil || *a.Segment == 0 || *a.Offset < 0 {
		return position{}, false, errors.New("spool ack file is corrupt")
	}
	if _, err := dec.Token(); err != io.EOF {
		return position{}, false, errors.New("spool ack file is corrupt")
	}
	return position{Seq: *a.Segment, Off: *a.Offset}, true, nil
}

// writeAck replaces the ack file atomically: a crash leaves the old or the
// new position, never a partial file.
func writeAck(dir string, p position) error {
	data, err := json.Marshal(ackFile{Segment: &p.Seq, Offset: &p.Off})
	if err != nil {
		return errors.New("spool ack file cannot be encoded")
	}
	tmp, err := os.CreateTemp(dir, ".ack-*.tmp")
	if err != nil {
		return errors.New("spool ack file cannot be written")
	}
	name := tmp.Name()
	_, writeErr := tmp.Write(data)
	syncErr := tmp.Sync()
	closeErr := tmp.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return errors.New("spool ack file cannot be written")
	}
	if err := retryOnWindows(func() error { return os.Rename(name, filepath.Join(dir, ackName)) }); err != nil {
		_ = os.Remove(name)
		return errors.New("spool ack file cannot be replaced")
	}
	syncDir(dir)
	return nil
}

// removeFile deletes path; a file that is already gone is not an error.
func removeFile(path string) error {
	return retryOnWindows(func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// retryOnWindows retries fn briefly on Windows, where a scanner or indexer
// can hold a file open for a moment.
func retryOnWindows(fn func() error) error {
	err := fn()
	if runtime.GOOS != "windows" {
		return err
	}
	for attempt := 0; err != nil && attempt < 5; attempt++ {
		time.Sleep(20 * time.Millisecond)
		err = fn()
	}
	return err
}

// syncDir makes renames and removals durable. Windows cannot sync a
// directory handle.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
