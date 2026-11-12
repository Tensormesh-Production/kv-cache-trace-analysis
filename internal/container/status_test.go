package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

func TestCollectorStatus(t *testing.T) {
	r := &Runtime{Bin: filepath.Join(t.TempDir(), "no-docker")}
	dir := t.TempDir()
	if _, err := r.CollectorStatus(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("want an error without a collector")
	}
	deadline := time.Unix(1794436800, 0)
	config, err := manifest.DockerConfig(resource.MustParse("5Gi"), deadline)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, TraceDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), config, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"request-trace.jsonl": 100, "request-trace-2026.jsonl": 50} {
		if err := os.WriteFile(filepath.Join(dir, TraceDir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := r.CollectorStatus(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "" || s.Files != 2 || s.Bytes != 150 || s.Newest.IsZero() || !s.Deadline.Equal(deadline) {
		t.Errorf("status %+v", s)
	}
}
