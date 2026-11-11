package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/kube"
)

func newStop(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Delete the trace collector and the trace files",
		Long: `Deletes the analyzer Jobs, the trace collector and its volume with all trace
files. Trace files hold request content, so run it after the analysis.

With the Tensormesh Platform, deletes the analyzer Jobs, sets the trace volume to
be deleted with its claim, and prints the settings and the helm command that turn
off request traces, which deletes the volume.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.useDocker() {
				return stopDocker(cmd.Context(), o)
			}
			return stopKube(cmd.Context(), o)
		},
	}
}

func stopKube(ctx context.Context, o *options) error {
	c, err := kube.New(o.kubeconfig, o.context, o.out)
	if err != nil {
		return err
	}
	t, err := findTarget(ctx, o, c)
	if err != nil {
		return err
	}
	if t.Kind == kube.Platform {
		return stopPlatform(ctx, o, c, t)
	}
	if err := o.confirm(fmt.Sprintf("Delete namespace %s with the trace collector and all trace files?", t.Namespace)); err != nil {
		return err
	}
	if o.dryRun {
		o.say("Would delete the analyzer Jobs, set the volume of %s to be deleted with it, and delete namespace %s.", kube.Volume, t.Namespace)
		return nil
	}
	if err := c.DeleteAnalyzers(ctx, t.Namespace); err != nil {
		return err
	}
	pv, err := c.DeleteVolumeWithClaim(ctx, t.Namespace, kube.Volume)
	if err != nil {
		return err
	}
	o.say("Deleting namespace %s and its trace files...", t.Namespace)
	if err := c.DeleteNamespace(ctx, t.Namespace, pv, 5*time.Minute); err != nil {
		return err
	}
	o.say("Deleted namespace %s and its trace volume.", t.Namespace)
	o.say("Remove the LMCache flags and the Dynamo environment variables from the trace sources.")
	return nil
}

func stopPlatform(ctx context.Context, o *options, c *kube.Client, t *kube.Target) error {
	r, err := c.FindRelease(ctx, t.Namespace)
	if err != nil {
		return err
	}
	if err := o.confirm(fmt.Sprintf("Delete the analyzer Jobs in %s, and set the trace volume %s to be deleted with its claim?", t.Namespace, t.Volume)); err != nil {
		return err
	}
	if !o.dryRun {
		if err := c.DeleteAnalyzers(ctx, t.Namespace); err != nil {
			return err
		}
		if _, err := c.DeleteVolumeWithClaim(ctx, t.Namespace, t.Volume); err != nil {
			return err
		}
		o.say("Deleted the analyzer Jobs. The trace volume is now deleted with its claim.")
	}
	if r == nil {
		o.say("Turn off request traces in the platform release: set observability.traces.requestTrace.enabled to false, and remove %s from engine.spec.extraArgs.", kube.TokenTraceFlag)
		return nil
	}
	o.say("\nTo turn off request traces, save these settings as %s:\n\n%s\n", traceOffValuesFile, traceOffValues)
	o.say("If engine.spec.extraArgs has other flags, keep them and remove only %s.", kube.TokenTraceFlag)
	o.say("If observability was off before the trace collection, also set observability.enabled: false.")
	o.say("Then upgrade the release. It deletes the trace volume, and the engine pods restart.\n\n  %s", kube.HelmCommand(r, traceOffValuesFile))
	return nil
}

// traceOffValuesFile and traceOffValues turn off the platform's request traces,
// as in the docs.
const (
	traceOffValuesFile = "trace-off-values.yaml"
	traceOffValues     = `observability:
  traces:
    requestTrace:
      enabled: false
engine:
  spec:
    extraArgs: []`
)

func stopDocker(ctx context.Context, o *options) error {
	rt, err := runtimeFor(ctx, o)
	if err != nil {
		return err
	}
	if err := o.confirm(fmt.Sprintf("Delete the %s container and all trace files in %s?", "trace-collector", traceDir(o.dir))); err != nil {
		return err
	}
	if err := rt.Stop(ctx, o.dir, o.dryRun); err != nil {
		return err
	}
	if !o.dryRun {
		o.say("Deleted the trace files in %s.", traceDir(o.dir))
		o.say("Remove the LMCache flags and the Dynamo environment variables from the trace sources.")
	}
	return nil
}
