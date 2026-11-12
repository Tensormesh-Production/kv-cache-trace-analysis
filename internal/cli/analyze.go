package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/container"
	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/kube"
	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

type analyzeOptions struct {
	format, customFormat, model, analyzerArgs string
	image, pull, memory, cpus, outputDir      string
	timeout                                   time.Duration
}

var formatNames = map[string]string{"lmcache": "LMCache", "dynamo": "Dynamo", "custom": "custom"}

func newAnalyze(o *options) *cobra.Command {
	a := &analyzeOptions{}
	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Run the analysis and save report-<format>.html",
		Long: `Runs the LMCache trace analyzer next to the trace files, and saves one HTML
report per trace format in the output folder. By default, it analyzes every
format that has records: LMCache and Dynamo (LMCache only with the Tensormesh
Platform). The analyzer runs with no network access and is deleted afterwards.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			formats, explicit, err := a.formats()
			if err != nil {
				return err
			}
			if o.useDocker() {
				return analyzeDocker(cmd.Context(), o, a, formats, explicit)
			}
			return analyzeKube(cmd.Context(), o, a, formats, explicit)
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.format, "format", "", "trace format: lmcache, dynamo or custom (default: every format with records)")
	f.StringVar(&a.customFormat, "custom-format", "", "custom_format.py for traces from other routers (sets --format custom)")
	f.StringVar(&a.model, "model", "", "analyze one model only")
	f.StringVar(&a.analyzerArgs, "analyzer-args", "", "more options for lmcache bench trace-analyze, such as \"--chunk-size 256\"")
	f.StringVar(&a.image, "image", "", "analyzer image, for a mirror or one LMCache version (default: the latest analyzer image)")
	f.StringVar(&a.pull, "pull", "always", "when to pull the analyzer image: always, missing or never")
	f.StringVar(&a.memory, "memory", "16Gi", "memory limit of the analyzer: about 0.4 GB plus 38 MB per million tokens")
	f.StringVar(&a.cpus, "cpus", "2", "CPU limit of the analyzer")
	f.StringVar(&a.outputDir, "output-dir", ".", "folder for the reports")
	f.DurationVar(&a.timeout, "timeout", time.Hour, "longest time for one analysis")
	return cmd
}

// formats returns the formats to analyze and whether the user chose them.
func (a *analyzeOptions) formats() ([]string, bool, error) {
	switch {
	case a.customFormat != "":
		if a.format != "" && a.format != "custom" {
			return nil, false, errors.New("--custom-format runs the custom format; leave out --format")
		}
		if _, err := os.Stat(a.customFormat); err != nil {
			return nil, false, fmt.Errorf("--custom-format: %w", err)
		}
		return []string{"custom"}, true, nil
	case a.format == "custom":
		return nil, false, errors.New("--format custom needs --custom-format custom_format.py")
	case a.format != "":
		if _, ok := formatNames[a.format]; !ok {
			return nil, false, fmt.Errorf("--format must be lmcache, dynamo or custom, not %q", a.format)
		}
		return []string{a.format}, true, nil
	}
	return []string{"lmcache", "dynamo"}, false, nil
}

func (a *analyzeOptions) args() string {
	args := a.analyzerArgs
	if a.model != "" {
		args = strings.TrimSpace("--model " + a.model + " " + args)
	}
	return args
}

func (a *analyzeOptions) pullPolicy() (corev1.PullPolicy, error) {
	switch a.pull {
	case "always":
		return corev1.PullAlways, nil
	case "missing":
		return corev1.PullIfNotPresent, nil
	case "never":
		return corev1.PullNever, nil
	}
	return "", fmt.Errorf("--pull must be always, missing or never, not %q", a.pull)
}

// noRecords reports whether the analyzer found no records in its format.
func noRecords(log []byte, format string) bool {
	return bytes.Contains(log, []byte(fmt.Sprintf("No '%s' trace records", format)))
}

func analyzeKube(ctx context.Context, o *options, a *analyzeOptions, formats []string, explicit bool) error {
	pull, err := a.pullPolicy()
	if err != nil {
		return err
	}
	memory, err := resource.ParseQuantity(a.memory)
	if err != nil {
		return fmt.Errorf("--memory: %w", err)
	}
	cpus, err := resource.ParseQuantity(a.cpus)
	if err != nil {
		return fmt.Errorf("--cpus: %w", err)
	}
	c, err := kube.New(o.kubeconfig, o.context, o.out)
	if err != nil {
		return err
	}
	t, err := findTarget(ctx, o, c)
	if err != nil {
		return err
	}
	if t.Kind == kube.Platform {
		if !explicit {
			formats = []string{"lmcache"}
		}
		running, err := c.PlatformCollectorRunning(ctx, t.Namespace)
		if err != nil {
			return err
		}
		if !running {
			return fmt.Errorf("the platform's OpenTelemetry Collector is not running in namespace %s", t.Namespace)
		}
	}
	if a.customFormat != "" && !o.dryRun {
		script, err := os.ReadFile(a.customFormat)
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{Data: map[string]string{"custom_format.py": string(script)}}
		cm.Name, cm.Namespace = kube.FormatConfigMap, t.Namespace
		if err := c.Apply(ctx, cm); err != nil {
			return err
		}
	}
	var reports []string
	for _, format := range formats {
		job, err := manifest.RenderAnalyzer(format, manifest.AnalyzerOptions{
			Namespace: t.Namespace, Image: a.image, PullPolicy: pull, AnalyzerArgs: a.args(),
			Memory: memory, CPU: cpus, Volume: t.Volume, Platform: t.Kind == kube.Platform,
		})
		if err != nil {
			return err
		}
		if o.dryRun {
			if err := printYAML(o.out, job); err != nil {
				return err
			}
			continue
		}
		o.say("Analyzing %s traces in namespace %s...", formatNames[format], t.Namespace)
		log, err := c.RunJob(ctx, job, a.timeout)
		path, err := handleResult(o, a, format, log, err, explicit)
		if err != nil {
			return err
		}
		if path != "" {
			reports = append(reports, path)
		}
	}
	return finishReports(o, reports, explicit)
}

func analyzeDocker(ctx context.Context, o *options, a *analyzeOptions, formats []string, explicit bool) error {
	if _, err := a.pullPolicy(); err != nil {
		return err
	}
	rt, err := runtimeFor(ctx, o)
	if err != nil {
		return err
	}
	image := a.image
	if image == "" {
		job, err := manifest.RenderAnalyzer("lmcache", manifest.AnalyzerOptions{})
		if err != nil {
			return err
		}
		image = job.Spec.Template.Spec.Containers[0].Image
	}
	var reports []string
	for _, format := range formats {
		if !o.dryRun {
			o.say("Analyzing %s traces in %s...", formatNames[format], traceDir(o.dir))
		}
		log, err := rt.Analyze(ctx, container.AnalyzeOptions{
			Dir: o.dir, Format: format, Image: image, Pull: a.pull, AnalyzerArgs: a.args(),
			Memory: dockerMemory(a.memory), CPUs: a.cpus, CustomFormat: a.customFormat, DryRun: o.dryRun,
		})
		if o.dryRun {
			if err != nil {
				return err
			}
			continue
		}
		var run *container.RunError
		switch {
		case errors.As(err, &run):
			err = &kube.JobError{Log: run.Log}
		case errors.Is(err, container.ErrOutOfMemory):
			err = kube.ErrOutOfMemory
		}
		path, err := handleResult(o, a, format, log, err, explicit)
		if err != nil {
			return err
		}
		if path != "" {
			reports = append(reports, path)
		}
	}
	return finishReports(o, reports, explicit)
}

// handleResult saves a report, or explains why there is none.
func handleResult(o *options, a *analyzeOptions, format string, log []byte, err error, explicit bool) (string, error) {
	var jobErr *kube.JobError
	switch {
	case err == nil:
		path := filepath.Join(a.outputDir, "report-"+format+".html")
		if err := os.WriteFile(path, log, 0o644); err != nil {
			return "", err
		}
		o.say("  Saved %s", path)
		return path, nil
	case errors.Is(err, kube.ErrOutOfMemory):
		return "", fmt.Errorf("the analyzer ran out of memory with --memory %s: raise --memory, or analyze one model with --model", a.memory)
	case errors.As(err, &jobErr) && noRecords(jobErr.Log, format):
		if a.model != "" {
			return "", fmt.Errorf("no %s trace records for model %s: check --model", formatNames[format], a.model)
		}
		if explicit {
			return "", fmt.Errorf("no %s trace records found: check --format and the trace source settings", formatNames[format])
		}
		o.say("  No %s trace records; skipped", formatNames[format])
		return "", nil
	case errors.As(err, &jobErr):
		return "", fmt.Errorf("the %s analysis failed:\n%s", formatNames[format], strings.TrimSpace(tailLines(string(jobErr.Log), 20)))
	}
	return "", err
}

func finishReports(o *options, reports []string, explicit bool) error {
	if o.dryRun {
		return nil
	}
	if len(reports) == 0 && !explicit {
		return errors.New("no trace records found: check the trace source settings, and that nothing blocks port 4317 to the collector")
	}
	if len(reports) > 0 {
		o.say("\nOpen a report in a browser: open %s on macOS, xdg-open %s on Linux.", reports[0], reports[0])
	}
	return nil
}

// findTarget picks the collector: the namespace given, else trace-analysis,
// else the Tensormesh Platform in tensormesh-platform.
func findTarget(ctx context.Context, o *options, c *kube.Client) (*kube.Target, error) {
	namespaces := []string{o.namespace}
	if o.namespace == "" {
		namespaces = []string{kitNamespace, kube.PlatformNamespace}
	}
	for _, ns := range namespaces {
		t, err := c.FindTarget(ctx, ns)
		if err != nil {
			return nil, err
		}
		if t.Kind != kube.None {
			return t, nil
		}
	}
	if o.namespace != "" {
		return nil, fmt.Errorf("no trace collector in namespace %s: run tm-trace start%s, or turn on the platform's request traces", o.namespace, nsFlag(o.namespace))
	}
	return nil, errors.New("no trace collector found: run tm-trace start first, or pass -n with the namespace of the collector")
}

// dockerMemory converts a Kubernetes quantity such as 16Gi to Docker's 16g.
func dockerMemory(q string) string {
	if v, err := resource.ParseQuantity(q); err == nil {
		return fmt.Sprintf("%db", v.Value())
	}
	return q
}
