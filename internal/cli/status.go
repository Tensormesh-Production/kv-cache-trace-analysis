package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/kube"
)

func newStatus(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show how much the trace collector has collected so far",
		Long: `Shows the state of the trace collector and how much it has collected: the
number of request records on Kubernetes, read from the collector's metrics, or
the size of the trace files with --docker. It changes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if o.useDocker() {
				return statusDocker(cmd.Context(), o)
			}
			return statusKube(cmd.Context(), o)
		},
	}
}

func statusKube(ctx context.Context, o *options) error {
	c, err := kube.New(o.kubeconfig, o.context, o.out)
	if err != nil {
		return err
	}
	t, err := findTarget(ctx, o, c)
	if err != nil {
		return err
	}
	s, err := c.CollectorStatus(ctx, t)
	if err != nil {
		return err
	}
	name := "Trace collector"
	if t.Kind == kube.Platform {
		name = "Tensormesh Platform collector"
	}
	if s.Pod == "" {
		o.say("%s in namespace %s: no collector pod.", name, t.Namespace)
	} else {
		o.say("%s in namespace %s: pod %s on node %s, %s%s.", name, t.Namespace, s.Pod, s.Node, s.Phase, since(s.Started))
	}
	if s.Counts != nil {
		o.say("Request records written since the collector started: %s spans (LMCache) and %s log records (NVIDIA Dynamo).",
			count(s.Counts.Spans), count(s.Counts.LogRecords))
		if s.Counts.AfterDeadline > 0 {
			o.say("Records dropped after the --duration deadline: %s.", count(s.Counts.AfterDeadline))
		}
	} else {
		o.say("Request records: not available (%v).", s.CountsError)
	}
	if s.VolumeSize != "" {
		o.say("Trace volume: %s, %s.", t.Volume, s.VolumeSize)
	}
	sayCollectionEnd(o, s.Deadline)
	o.say("For the report, run: tm-trace analyze%s", nsFlag(t.Namespace))
	return nil
}

func statusDocker(ctx context.Context, o *options) error {
	rt, err := runtimeFor(ctx, o)
	if err != nil {
		return err
	}
	s, err := rt.CollectorStatus(ctx, o.dir)
	if err != nil {
		return err
	}
	if s.State == "" {
		o.say("Trace collector: no %s container.", rt.Bin)
	} else {
		o.say("Trace collector (%s): %s%s.", rt.Bin, s.State, since(s.Started))
	}
	if s.Files == 0 {
		o.say("Trace files in %s: none yet.", traceDir(o.dir))
	} else {
		files := "files"
		if s.Files == 1 {
			files = "file"
		}
		o.say("Trace files in %s: %s in %d %s, newest written %s.", traceDir(o.dir), size(s.Bytes), s.Files, files, s.Newest.Format("2006-01-02 15:04 MST"))
	}
	sayCollectionEnd(o, s.Deadline)
	o.say("For the report, run: tm-trace analyze --docker%s", dirFlag(o.dir))
	return nil
}

func sayCollectionEnd(o *options, deadline time.Time) {
	switch {
	case deadline.IsZero():
	case time.Now().After(deadline):
		o.say("Collection stopped at %s (--duration).", deadline.Format("2006-01-02 15:04 MST"))
	default:
		o.say("Collection stops at %s (--duration).", deadline.Format("2006-01-02 15:04 MST"))
	}
}

func since(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return " since " + t.Local().Format("2006-01-02 15:04 MST")
}

// size formats bytes in MiB below 1 GiB, else in GiB.
func size(b int64) string {
	if b < 1<<30 {
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	}
	return gib(b)
}

func count(v float64) string {
	return fmt.Sprintf("%.0f", v)
}
