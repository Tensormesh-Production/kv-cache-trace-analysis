package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/container"
	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/kube"
	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

type startOptions struct {
	maxSize, storageClass, image, listen string
	nodeSelector                         map[string]string
}

// traceValuesFile and traceOnValues turn on the platform's request traces,
// as in the docs: the chart does not add the LMCache flag by itself.
const (
	traceValuesFile = "trace-values.yaml"
	traceOnValues   = `observability:
  enabled: true
  traces:
    requestTrace:
      enabled: true
engine:
  spec:
    extraArgs:
      - --enable-token-trace`
)

func newStart(o *options) *cobra.Command {
	s := &startOptions{}
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Deploy the trace collector and print the trace source settings",
		Long: `Deploys the trace collector, waits until it is ready, and prints the settings
for NVIDIA Dynamo and LMCache that send request traces to it.

With the Tensormesh Platform, prints the settings and the helm command that turn
on its request traces instead: the platform release is never changed by tm-trace.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			size, err := resource.ParseQuantity(s.maxSize)
			if err != nil {
				return fmt.Errorf("--max-size: %w", err)
			}
			if _, err := manifest.Backups(size); err != nil {
				return fmt.Errorf("--max-size: %w", err)
			}
			if o.useDocker() {
				return startDocker(cmd.Context(), o, s, size)
			}
			return startKube(cmd.Context(), o, s, size)
		},
	}
	f := cmd.Flags()
	f.StringVar(&s.maxSize, "max-size", manifest.DefaultSize, "most disk space for trace files: the volume size on Kubernetes; with --docker, start needs this much free space (minimum 5Gi)")
	f.StringVar(&s.storageClass, "storage-class", "", "StorageClass of the trace volume (default: the cluster's default)")
	f.StringToStringVar(&s.nodeSelector, "node-selector", nil, "run the collector on nodes with these labels, such as kubernetes.io/hostname=node-1")
	f.StringVar(&s.image, "collector-image", "", "collector image, for a mirror (default: the OpenTelemetry Collector Contrib image)")
	f.StringVar(&s.listen, "listen", "0.0.0.0", "host address for the collector ports, with --docker: 127.0.0.1 accepts traces from this host only")
	return cmd
}

func startKube(ctx context.Context, o *options, s *startOptions, size resource.Quantity) error {
	if o.dryRun && o.namespace != "" {
		return dryRunCollector(o, s, size, o.namespace)
	}
	c, err := kube.New(o.kubeconfig, o.context, o.out)
	if err != nil {
		return err
	}
	ns := o.namespace
	if ns == "" {
		ns = kitNamespace
		if r, err := c.FindRelease(ctx, kube.PlatformNamespace); err == nil && r != nil {
			return startPlatform(ctx, o, c, r)
		}
	} else if r, err := c.FindRelease(ctx, ns); err != nil {
		return err
	} else if r != nil {
		return startPlatform(ctx, o, c, r)
	}
	if o.dryRun {
		return dryRunCollector(o, s, size, ns)
	}
	col, err := manifest.RenderCollector(collectorOptions(s, size, ns))
	if err != nil {
		return err
	}
	if err := c.EnsureNamespace(ctx, ns); err != nil {
		return err
	}
	for _, obj := range col.Objects() {
		if err := c.Apply(ctx, obj); err != nil {
			return err
		}
	}
	o.say("Waiting for the trace collector in namespace %s...", ns)
	if err := c.WaitDeployment(ctx, ns, kube.Collector, 5*time.Minute); err != nil {
		return err
	}
	o.say("The trace collector is ready. Its trace files stay under %s (--max-size).\n", size.String())
	printSourceSettings(o, fmt.Sprintf("http://%s.%s.svc:4317", kube.Collector, ns))
	o.say("\nAfter the collection period, run: tm-trace analyze%s", nsFlag(ns))
	return nil
}

func collectorOptions(s *startOptions, size resource.Quantity, ns string) manifest.CollectorOptions {
	return manifest.CollectorOptions{
		Namespace:    ns,
		Size:         size,
		StorageClass: s.storageClass,
		NodeSelector: s.nodeSelector,
		Image:        s.image,
	}
}

func dryRunCollector(o *options, s *startOptions, size resource.Quantity, ns string) error {
	col, err := manifest.RenderCollector(collectorOptions(s, size, ns))
	if err != nil {
		return err
	}
	nsObj := &corev1.Namespace{}
	nsObj.Name = ns
	return printYAML(o.out, append([]any{nsObj}, toAny(col.Objects())...)...)
}

func startPlatform(ctx context.Context, o *options, c *kube.Client, r *kube.Release) error {
	o.say("Found the Tensormesh Platform: release %s, chart %s, namespace %s.", r.Name, r.Version, r.Namespace)
	volume, err := c.RequestTraceVolume(ctx, r.Namespace)
	if err != nil {
		return err
	}
	engines, err := c.Engines(ctx, r.Namespace)
	if err != nil {
		return err
	}
	for _, e := range engines {
		if e.Old {
			o.say("Warning: engine %s runs %s. Request traces need LMCache 0.5.7 or later: set engine.spec.image.tag first.", e.Name, e.Image)
		}
	}
	missing := 0
	for _, e := range engines {
		if !e.TokenTrace {
			missing++
		}
	}
	if volume != "" && missing == 0 && len(engines) > 0 {
		o.say("Request traces are on. After the collection period, run: tm-trace analyze -n %s", r.Namespace)
		return nil
	}
	o.say("\nTo turn on request traces, save these settings as %s:\n\n%s\n", traceValuesFile, traceOnValues)
	o.say("If engine.spec.extraArgs is already set in the release, add %s to that list and keep the other flags.", kube.TokenTraceFlag)
	o.say("Then upgrade the release. The engine pods restart.\n\n  %s\n", kube.HelmCommand(r, traceValuesFile))
	o.say("After the collection period, run: tm-trace analyze -n %s", r.Namespace)
	return nil
}

func startDocker(ctx context.Context, o *options, s *startOptions, size resource.Quantity) error {
	rt, err := runtimeFor(ctx, o)
	if err != nil {
		return err
	}
	config, err := manifest.DockerConfig(size)
	if err != nil {
		return err
	}
	image := s.image
	if image == "" {
		col, err := manifest.RenderCollector(collectorOptions(s, size, kitNamespace))
		if err != nil {
			return err
		}
		image = col.Deployment.Spec.Template.Spec.Containers[0].Image
	}
	if err := rt.StartCollector(ctx, container.CollectorOptions{
		Dir: o.dir, Image: image, Listen: s.listen, Config: config, MaxSize: size.Value(), DryRun: o.dryRun,
	}); err != nil {
		return err
	}
	if o.dryRun {
		return nil
	}
	host, _ := os.Hostname()
	o.say("The trace collector is ready. Its trace files in %s stay under %s (--max-size).\n", traceDir(o.dir), size.String())
	printSourceSettings(o, fmt.Sprintf("http://%s:4317", host))
	o.say("\nUse localhost instead of %s for sources on this host with host networking.", host)
	o.say("\nAfter the collection period, run: tm-trace analyze --docker%s", dirFlag(o.dir))
	return nil
}

func printSourceSettings(o *options, endpoint string) {
	o.say(`Send traces to it:

  LMCache: add these flags to the lmcache server command, or to
  lmcache.mp.autostart.server_args when vLLM starts the server:

    --enable-token-trace --otlp-endpoint %[1]s

  NVIDIA Dynamo: set these environment variables on the frontend:

    DYN_REQUEST_TRACE=1
    DYN_REQUEST_TRACE_RECORDS=request_payload
    DYN_REQUEST_TRACE_SINKS=otel
    OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=%[1]s
    OTEL_BLRP_MAX_EXPORT_BATCH_SIZE=8`, endpoint)
}
