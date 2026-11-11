package container

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

// Status is what the collector container has collected so far.
type Status struct {
	// State is the container state, such as running, or "" without a container.
	State    string
	Started  time.Time
	Files    int
	Bytes    int64
	Newest   time.Time
	Deadline time.Time
}

// CollectorStatus reports the collector container and the trace files in
// <dir>/traces.
func (r *Runtime) CollectorStatus(ctx context.Context, dir string) (*Status, error) {
	s := &Status{}
	if out, err := r.output(ctx, "inspect", "--format", "{{.State.Status}} {{.State.StartedAt}}", Collector); err == nil {
		state, started, _ := strings.Cut(strings.TrimSpace(out), " ")
		s.State = state
		s.Started, _ = time.Parse(time.RFC3339Nano, started)
	}
	if config, err := os.ReadFile(filepath.Join(dir, ConfigFile)); err == nil {
		s.Deadline, _ = manifest.Deadline(string(config))
	}
	traces := filepath.Join(dir, TraceDir)
	err := filepath.WalkDir(traces, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		s.Files++
		s.Bytes += info.Size()
		if info.ModTime().After(s.Newest) {
			s.Newest = info.ModTime()
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		if s.State == "" {
			return nil, errors.New("no trace collector found: run tm-trace start --docker first")
		}
		err = nil
	}
	return s, err
}
