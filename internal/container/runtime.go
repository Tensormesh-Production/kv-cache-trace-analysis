// Package container runs the collector and the analyzer with Docker or
// Podman on one host, without Kubernetes.
package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	// Collector is the collector container name.
	Collector = "trace-collector"
	// ConfigFile and TraceDir are created in the working folder.
	ConfigFile = "collector-config.yaml"
	TraceDir   = "traces"
)

// Runtime is the docker or podman command.
type Runtime struct {
	Bin string
	// Rootless podman also needs --userns=keep-id, so that --user maps to the
	// same user on the host.
	Rootless bool
	// NoLimits: rootless podman without the cpu and memory cgroup controllers
	// cannot set --cpus and --memory.
	NoLimits bool
	Out      io.Writer
}

// Detect finds the container runtime: want if set, else docker, else podman.
func Detect(ctx context.Context, want string, out io.Writer) (*Runtime, error) {
	candidates := []string{"docker", "podman"}
	if want != "" {
		candidates = []string{want}
	}
	for _, bin := range candidates {
		if _, err := exec.LookPath(bin); err != nil {
			continue
		}
		r := &Runtime{Bin: bin, Out: out}
		if _, err := r.output(ctx, "info", "--format", "{{json .}}"); err != nil {
			return nil, fmt.Errorf("%s is installed but not running: %w", bin, err)
		}
		if bin == "podman" {
			rootless, _ := r.output(ctx, "info", "--format", "{{.Host.Security.Rootless}}")
			r.Rootless = strings.TrimSpace(rootless) == "true"
			controllers, _ := r.output(ctx, "info", "--format", "{{.Host.CgroupControllers}}")
			r.NoLimits = r.Rootless && !(strings.Contains(controllers, "cpu") && strings.Contains(controllers, "memory"))
			if r.NoLimits {
				fmt.Fprintln(out, "Warning: rootless podman here cannot limit CPU and memory; the containers run without limits.")
			}
		}
		return r, nil
	}
	if want != "" {
		return nil, fmt.Errorf("%s not found in PATH", want)
	}
	return nil, errors.New("neither docker nor podman found in PATH")
}

// CollectorOptions are the user's settings for the collector container.
type CollectorOptions struct {
	Dir    string
	Image  string
	Listen string
	Config []byte
	// MaxSize is --max-size: the trace files stay under it, and start needs
	// this much free space.
	MaxSize int64
	DryRun  bool
}

// StartCollector starts the collector container, which writes the trace
// files to <dir>/traces as the current user.
func (r *Runtime) StartCollector(ctx context.Context, o CollectorOptions) error {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return err
	}
	traces := filepath.Join(dir, TraceDir)
	args := []string{"run", "-d", "--name", Collector, "--restart", "unless-stopped"}
	args = append(args, r.userArgs()...)
	args = append(args, r.limits("2", "2g")...)
	for _, port := range []string{"4317", "4318", "8889"} {
		args = append(args, "-p", publish(o.Listen, port))
	}
	args = append(args,
		"-v", filepath.Join(dir, ConfigFile)+":/etc/otelcol-contrib/config.yaml:ro"+relabel(),
		"-v", traces+":/data"+relabelRW(),
		o.Image)
	if o.DryRun {
		fmt.Fprintf(r.Out, "mkdir -m 700 -p %s\n# write %s\n%s %s\n", traces, filepath.Join(dir, ConfigFile), r.Bin, strings.Join(quote(args), " "))
		return nil
	}
	if state, _ := r.state(ctx); state != "" {
		return fmt.Errorf("a container named %s already exists (%s); run tm-trace stop --docker first", Collector, state)
	}
	if free, err := FreeBytes(existingDir(traces)); err == nil && free < o.MaxSize {
		return fmt.Errorf("the disk of %s has %s free, less than --max-size (%s); free some space or use a smaller --max-size (minimum 5Gi)",
			dir, gib(free), gib(o.MaxSize))
	}
	if err := os.MkdirAll(traces, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(traces, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ConfigFile), o.Config, 0o644); err != nil {
		return err
	}
	if _, err := r.output(ctx, args...); err != nil {
		return err
	}
	return r.waitReady(ctx, time.Minute)
}

// AnalyzeOptions are the user's settings for one analysis.
type AnalyzeOptions struct {
	Dir          string
	Format       string
	Image        string
	Pull         string
	AnalyzerArgs string
	Memory       string
	CPUs         string
	CustomFormat string
	DryRun       bool
}

// Analyze runs the analyzer container with no network and returns its
// output, which is the HTML report when it succeeds.
func (r *Runtime) Analyze(ctx context.Context, o AnalyzeOptions) ([]byte, error) {
	dir, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, err
	}
	traces := filepath.Join(dir, TraceDir)
	args := []string{"run", "--rm", "--pull", o.Pull, "--network", "none"}
	args = append(args, r.userArgs()...)
	args = append(args, r.limits(o.CPUs, o.Memory)...)
	args = append(args, "-v", traces+":/data:ro"+relabel(),
		"-e", "TRACE_FORMAT="+o.Format, "-e", "ANALYZER_ARGS="+o.AnalyzerArgs)
	if o.CustomFormat != "" {
		f, err := filepath.Abs(o.CustomFormat)
		if err != nil {
			return nil, err
		}
		args = append(args, "-v", f+":/formats/custom_format.py:ro"+relabel())
	}
	args = append(args, o.Image)
	if o.DryRun {
		fmt.Fprintf(r.Out, "%s %s > report-%s.html\n", r.Bin, strings.Join(quote(args), " "), o.Format)
		return nil, nil
	}
	if _, err := os.Stat(traces); err != nil {
		return nil, fmt.Errorf("no trace folder at %s: run tm-trace start --docker first, or set --dir", traces)
	}
	cmd := r.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = run(cmd)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == 137 {
			return stdout.Bytes(), ErrOutOfMemory
		}
		if stdout.Len() == 0 {
			return nil, fmt.Errorf("%s run: %s", r.Bin, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), &RunError{Log: stdout.Bytes()}
	}
	if err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// ErrOutOfMemory: the analyzer container ran out of memory.
var ErrOutOfMemory = errors.New("the analyzer ran out of memory")

// RunError is a failed analysis, with the analyzer's log.
type RunError struct{ Log []byte }

func (e *RunError) Error() string { return "the analyzer failed" }

// Stop removes the collector container and the trace files in dir.
func (r *Runtime) Stop(ctx context.Context, dir string, dryRun bool) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(r.Out, "%s rm -f %s\nrm -rf %s %s\n", r.Bin, Collector,
			filepath.Join(dir, TraceDir), filepath.Join(dir, ConfigFile))
		return nil
	}
	if state, _ := r.state(ctx); state != "" {
		if _, err := r.output(ctx, "rm", "-f", Collector); err != nil {
			return err
		}
		fmt.Fprintf(r.Out, "Removed the %s container\n", Collector)
	}
	if err := os.RemoveAll(filepath.Join(dir, TraceDir)); err != nil {
		return fmt.Errorf("delete the trace files: %w", err)
	}
	_ = os.Remove(filepath.Join(dir, ConfigFile))
	_ = os.Remove(dir)
	return nil
}

// Running reports whether the collector container runs.
func (r *Runtime) Running(ctx context.Context) bool {
	state, _ := r.state(ctx)
	return state == "running"
}

func (r *Runtime) state(ctx context.Context) (string, error) {
	out, err := r.output(ctx, "inspect", "--format", "{{.State.Status}}", Collector)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// waitReady waits for the collector's ready message in its log.
func (r *Runtime) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		log, _ := r.combined(ctx, "logs", Collector)
		if strings.Contains(log, "Everything is ready") {
			return nil
		}
		if state, _ := r.state(ctx); state == "exited" || state == "dead" {
			return fmt.Errorf("the collector stopped:\n%s", tail(log, 20))
		}
		time.Sleep(time.Second)
	}
	log, _ := r.combined(ctx, "logs", Collector)
	return fmt.Errorf("the collector is not ready after %s:\n%s", timeout, tail(log, 20))
}

// userArgs run the container as the current user, so the trace files belong
// to that user. Windows has no user ids; Docker Desktop handles the files.
func (r *Runtime) userArgs() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	user := []string{"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
	if r.Rootless {
		// keep-id maps the current user to the same ids in the container;
		// --user then runs the process as that user instead of the image's.
		return append([]string{"--userns=keep-id"}, user...)
	}
	return user
}

func (r *Runtime) limits(cpus, memory string) []string {
	if r.NoLimits {
		return nil
	}
	return []string{"--cpus", cpus, "--memory", memory}
}

func (r *Runtime) output(ctx context.Context, args ...string) (string, error) {
	cmd := r.command(ctx, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := run(cmd); err != nil {
		return "", fmt.Errorf("%s %s: %s", r.Bin, args[0], strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (r *Runtime) combined(ctx context.Context, args ...string) (string, error) {
	cmd := r.command(ctx, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := run(cmd)
	return out.String(), err
}

// command runs the container runtime. Podman's helper processes, such as
// conmon and rootlessport, keep its output pipes open after podman run -d
// exits, so Wait stops waiting for them soon after the command ends.
func (r *Runtime) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// run runs cmd. Pipes left open by its helper processes are not an error.
func run(cmd *exec.Cmd) error {
	if err := cmd.Run(); !errors.Is(err, exec.ErrWaitDelay) {
		return err
	}
	return nil
}

func publish(listen, port string) string {
	if listen == "" || listen == "0.0.0.0" {
		return port + ":" + port
	}
	return listen + ":" + port + ":" + port
}

// relabel adds the SELinux option on hosts that enforce SELinux, where
// containers cannot read a bind mount without it.
func relabel() string {
	if selinuxEnforcing() {
		return ",z"
	}
	return ""
}

func relabelRW() string {
	if selinuxEnforcing() {
		return ":z"
	}
	return ""
}

func selinuxEnforcing() bool {
	b, err := os.ReadFile("/sys/fs/selinux/enforce")
	return err == nil && strings.TrimSpace(string(b)) == "1"
}

// existingDir returns path, or its nearest parent that exists, so that the free
// space is checked before anything is created.
func existingDir(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// quote makes the printed command copyable.
func quote(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \"'$") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return out
}
