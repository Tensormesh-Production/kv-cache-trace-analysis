package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/container"
)

// signalContext is cancelled on Ctrl+C, so that running Jobs are cleaned up.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runtimeFor(ctx context.Context, o *options) (*container.Runtime, error) {
	if o.dryRun {
		bin := o.runtime
		if bin == "" {
			bin = "docker"
		}
		return &container.Runtime{Bin: bin, Out: o.out}, nil
	}
	return container.Detect(ctx, o.runtime, o.out)
}

// printYAML prints objects as a multi-document YAML manifest.
func printYAML(out io.Writer, objs ...any) error {
	for _, obj := range objs {
		if ro, ok := obj.(runtime.Object); ok {
			if gvks, _, err := scheme.Scheme.ObjectKinds(ro); err == nil {
				ro.GetObjectKind().SetGroupVersionKind(gvks[0])
			}
		}
		b, err := yaml.Marshal(obj)
		if err != nil {
			return err
		}
		var lines []string
		for _, l := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(l)
			if t == "creationTimestamp: null" || t == "status: {}" {
				continue
			}
			lines = append(lines, l)
		}
		fmt.Fprintf(out, "---\n%s\n", strings.TrimRight(strings.Join(lines, "\n"), "\n"))
	}
	return nil
}

func toAny(objs []runtime.Object) []any {
	out := make([]any, len(objs))
	for i, o := range objs {
		out[i] = o
	}
	return out
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func nsFlag(ns string) string {
	if ns == kitNamespace {
		return ""
	}
	return " -n " + ns
}

func dirFlag(dir string) string {
	if dir == "tm-trace" {
		return ""
	}
	return " --dir " + dir
}

func traceDir(dir string) string {
	return filepath.Join(dir, container.TraceDir)
}

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
