package inbound

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/lhns/matrix-sip-bridge/pkg/database"
)

const (
	spoolSuffix = ".json"
	badSuffix   = ".bad"
	tmpPrefix   = ".tmp-"
)

// Spool is a directory of accepted texts that could not be written to the
// database: one JSON file per message, named so that a sorted listing is the
// order they were received in.
type Spool struct {
	dir string
	seq atomic.Uint64
}

// NewSpool creates the directory if needed.
func NewSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	return &Spool{dir: dir}, nil
}

// Write stores m durably: temp file, fsync, atomic rename, fsync of the
// directory. When it returns nil the message survives a crash.
func (s *Spool) Write(m *database.InboundMessage) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Receive time first, then a per-process sequence for messages in the same
	// nanosecond, then the UID so names never collide across restarts.
	name := fmt.Sprintf("%020d-%06d-%s%s", m.ReceivedAt.UnixNano(), s.seq.Add(1)%1_000_000, m.UID, spoolSuffix)
	tmp, err := os.CreateTemp(s.dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return s.syncDir()
}

// syncDir makes the rename durable. Windows cannot fsync a directory, so there
// the error is ignored and the guarantee is the platform's.
func (s *Spool) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

// List returns the file names waiting, oldest first.
func (s *Spool) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), spoolSuffix) && !strings.HasPrefix(e.Name(), tmpPrefix) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	return names, nil
}

// Read parses one file.
func (s *Spool) Read(name string) (*database.InboundMessage, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return nil, err
	}
	var m database.InboundMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.UID == "" {
		return nil, errors.New("no uid")
	}
	return &m, nil
}

// Remove deletes a file that is in the database.
func (s *Spool) Remove(name string) error {
	return os.Remove(filepath.Join(s.dir, name))
}

// Quarantine sets aside a file that cannot be read, so it stops being retried
// and is not lost.
func (s *Spool) Quarantine(name string) error {
	return os.Rename(filepath.Join(s.dir, name), filepath.Join(s.dir, name+badSuffix))
}
