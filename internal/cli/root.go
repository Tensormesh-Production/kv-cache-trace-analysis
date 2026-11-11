// Package cli is the tm-trace command line.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Version is set at build time.
var Version = "dev"

const kitNamespace = "trace-analysis"

type options struct {
	kubeconfig, context, namespace string
	docker                         bool
	runtime, dir                   string
	dryRun, yes                    bool
	out                            io.Writer
	in                             io.Reader
}

// docker reports whether to use containers instead of Kubernetes.
func (o *options) useDocker() bool { return o.docker || o.runtime != "" }

// NewRoot builds the tm-trace command.
func NewRoot(out io.Writer, in io.Reader) *cobra.Command {
	o := &options{out: out, in: in}
	root := &cobra.Command{
		Use:   "tm-trace",
		Short: "Measure how much of an LLM workload a KV cache can reuse",
		Long: `tm-trace collects request traces from NVIDIA Dynamo or LMCache and analyzes
them with the LMCache trace analyzer. The result is an HTML report of prefix and
non-prefix hit rates for a range of cache sizes. The trace files stay in your
cluster or on your host, and are deleted by tm-trace stop.

  tm-trace start     deploy the trace collector and print the trace source settings
  tm-trace status    show how much the collector has collected so far
  tm-trace analyze   run the analysis and save report-<format>.html
  tm-trace stop      delete the collector and the trace files

Kubernetes is the default. With the Tensormesh Platform, use its namespace
(found automatically in tensormesh-platform). Without Kubernetes, add --docker.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	f := root.PersistentFlags()
	f.StringVar(&o.kubeconfig, "kubeconfig", "", "kubeconfig file (default: KUBECONFIG or ~/.kube/config)")
	f.StringVar(&o.context, "context", "", "kubeconfig context (default: the current context)")
	f.StringVarP(&o.namespace, "namespace", "n", "", "namespace (default: trace-analysis, or tensormesh-platform with the platform)")
	f.BoolVar(&o.docker, "docker", false, "use Docker or Podman on this host instead of Kubernetes")
	f.StringVar(&o.runtime, "runtime", "", "container runtime for --docker: docker or podman (default: the one installed)")
	f.StringVar(&o.dir, "dir", "tm-trace", "folder for the collector configuration and trace files, with --docker")
	f.BoolVar(&o.dryRun, "dry-run", false, "print what would be created or run, and change nothing")
	f.BoolVarP(&o.yes, "yes", "y", false, "do not ask for confirmation")
	root.AddCommand(newStart(o), newStatus(o), newAnalyze(o), newStop(o), newVersion(o))
	return root
}

// Execute runs tm-trace and returns the exit code.
func Execute() int {
	ctx, cancel := signalContext()
	defer cancel()
	if err := NewRoot(os.Stdout, os.Stdin).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}

func (o *options) say(format string, args ...any) {
	fmt.Fprintf(o.out, format+"\n", args...)
}

// confirm asks a yes/no question, unless --yes or --dry-run is set.
func (o *options) confirm(question string) error {
	if o.yes || o.dryRun {
		return nil
	}
	if f, ok := o.in.(*os.File); !ok || !term.IsTerminal(int(f.Fd())) {
		return errors.New(question + " Add --yes to confirm")
	}
	fmt.Fprintf(o.out, "%s [y/N] ", question)
	line, _ := bufio.NewReader(o.in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("cancelled")
	}
	return nil
}

func newVersion(o *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.say("tm-trace %s", Version)
			return nil
		},
	}
}
