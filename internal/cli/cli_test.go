package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestFormats(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom_format.py")
	if err := os.WriteFile(custom, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		a        analyzeOptions
		want     string
		explicit bool
		err      bool
	}{
		{analyzeOptions{}, "lmcache,dynamo", false, false},
		{analyzeOptions{format: "dynamo"}, "dynamo", true, false},
		{analyzeOptions{customFormat: custom}, "custom", true, false},
		{analyzeOptions{customFormat: custom, format: "custom"}, "custom", true, false},
		{analyzeOptions{customFormat: custom, format: "lmcache"}, "", false, true},
		{analyzeOptions{format: "custom"}, "", false, true},
		{analyzeOptions{format: "other"}, "", false, true},
		{analyzeOptions{customFormat: "missing.py"}, "", false, true},
	} {
		got, explicit, err := c.a.formats()
		if (err != nil) != c.err || strings.Join(got, ",") != c.want || explicit != c.explicit {
			t.Errorf("%+v: %v %v %v", c.a, got, explicit, err)
		}
	}
}

func TestArgsAndConversions(t *testing.T) {
	a := analyzeOptions{model: "llama", analyzerArgs: "--chunk-size 256"}
	if got := a.args(); got != "--model llama --chunk-size 256" {
		t.Errorf("args %q", got)
	}
	if got := dockerMemory("16Gi"); got != "17179869184b" {
		t.Errorf("dockerMemory %q", got)
	}
	if !noRecords([]byte("ValueError: No 'dynamo' trace records in /data/a.jsonl"), "dynamo") {
		t.Error("noRecords: want true")
	}
	if noRecords([]byte("No 'lmcache' trace records"), "dynamo") {
		t.Error("noRecords: other format")
	}
}

// The dry runs print valid manifests and commands without a cluster or a daemon.
func TestDryRuns(t *testing.T) {
	run := func(args ...string) string {
		var out bytes.Buffer
		cmd := NewRoot(&out, strings.NewReader(""))
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	manifests := run("start", "--dry-run", "-n", "test", "--max-size", "10Gi")
	for _, doc := range strings.Split(manifests, "---\n")[1:] {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil || m["kind"] == nil {
			t.Fatalf("bad document %q: %v", doc, err)
		}
	}
	for _, want := range []string{"kind: Namespace", "max_backups: 16", "storage: 10Gi", "namespace: test"} {
		if !strings.Contains(manifests, want) {
			t.Errorf("start --dry-run: missing %q", want)
		}
	}
	dir := t.TempDir()
	docker := run("start", "--docker", "--dry-run", "--dir", dir, "--listen", "127.0.0.1")
	if !strings.Contains(docker, "-p 127.0.0.1:4317:4317") || !strings.Contains(docker, "--restart unless-stopped") {
		t.Errorf("start --docker --dry-run:\n%s", docker)
	}
	analyze := run("analyze", "--docker", "--dry-run", "--dir", dir, "--format", "dynamo")
	if !strings.Contains(analyze, "--network none") || !strings.Contains(analyze, "TRACE_FORMAT=dynamo") {
		t.Errorf("analyze --docker --dry-run:\n%s", analyze)
	}
	if out := run("stop", "--docker", "--dry-run", "--dir", dir); !strings.Contains(out, "rm -f trace-collector") {
		t.Errorf("stop --docker --dry-run:\n%s", out)
	}
	if out := run("version"); !strings.HasPrefix(out, "tm-trace ") {
		t.Errorf("version: %q", out)
	}
}

func TestMaxSize(t *testing.T) {
	for _, c := range []struct{ size, err string }{
		{"4Gi", "--max-size: size 4Gi is below the minimum of 5Gi"},
		{"lots", "--max-size: quantities must match"},
	} {
		cmd := NewRoot(&bytes.Buffer{}, strings.NewReader(""))
		cmd.SetArgs([]string{"start", "--dry-run", "-n", "test", "--max-size", c.size})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("--max-size %s: %v", c.size, err)
		}
	}
}

func TestDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90m": 90 * time.Minute, "12h": 12 * time.Hour, "2d": 48 * time.Hour, "1s": time.Second} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"0", "-1h", "2x", "d", "1.5d"} {
		if _, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q): want an error", in)
		}
	}
	var out bytes.Buffer
	cmd := NewRoot(&out, strings.NewReader(""))
	cmd.SetArgs([]string{"start", "--dry-run", "-n", "test", "--duration", "2h"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "filter/deadline") || !strings.Contains(out.String(), "UnixSeconds(Now()) > ") {
		t.Errorf("start --duration --dry-run:\n%s", out.String())
	}
}

func TestPlatformValues(t *testing.T) {
	for _, v := range []string{traceOnValues, traceOffValues} {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(v), &m); err != nil || m["observability"] == nil || m["engine"] == nil {
			t.Errorf("bad values %q: %v", v, err)
		}
	}
	if !strings.Contains(traceOnValues, "- --enable-token-trace") || !strings.Contains(traceOffValues, "extraArgs: []") {
		t.Error("values do not set the LMCache flag")
	}
}
