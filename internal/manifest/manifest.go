// Package manifest renders the embedded collector and analyzer manifests with
// the user's options. With default options, the output equals the files in
// deploy/, which the README steps and the tests use.
package manifest

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"

	kit "github.com/tensormesh-production/kv-cache-trace-analysis"
)

const (
	// FileSize is the collector's rotation size: 512 MiB per trace file.
	FileSize = 512 << 20
	// DefaultSize is the default trace storage: a 50 GiB volume, 80 rotated files.
	DefaultSize = "50Gi"
	// MinSize is the smallest trace storage the collector is configured for.
	MinSize = 5 << 30

	defaultBackups = "max_backups: 80"

	// PlatformCollectorLabel selects the platform's collector pod. The OTel
	// Operator sets it on every collector pod it creates.
	PlatformCollectorLabel = "app.kubernetes.io/component"
	// PlatformCollectorValue is the value of PlatformCollectorLabel.
	PlatformCollectorValue = "opentelemetry-collector"

	// KServePort and KServePath are where the collector's KServe input
	// receives the requests of KServe's payload logger.
	KServePort = 8088
	KServePath = "/events"
	// KServeReceiver is the collector's KServe input.
	KServeReceiver = "webhook_event/kserve"
)

// Formats are the trace formats with an analyzer manifest.
var Formats = []string{"lmcache", "dynamo", "custom"}

// Backups returns how many rotated trace files fit in size: 1.6 per GiB,
// so 80 for 50 GiB. The collector then keeps at most (Backups+1) x 512 MiB.
func Backups(size resource.Quantity) (int, error) {
	b := size.Value()
	if b < MinSize {
		return 0, fmt.Errorf("size %s is below the minimum of 5Gi", size.String())
	}
	return int(b * 16 / (10 << 30)), nil
}

// MaxUsage is the most disk space the collector's trace files take for size,
// which is below size (--max-size).
func MaxUsage(size resource.Quantity) (int64, error) {
	n, err := Backups(size)
	if err != nil {
		return 0, err
	}
	return int64(n+1) * FileSize, nil
}

// Collector holds the trace collector objects for Kubernetes.
type Collector struct {
	ConfigMap  *corev1.ConfigMap
	Volume     *corev1.PersistentVolumeClaim
	Deployment *appsv1.Deployment
	Service    *corev1.Service
}

// Objects returns the collector objects in apply order.
func (c *Collector) Objects() []runtime.Object {
	return []runtime.Object{c.ConfigMap, c.Volume, c.Deployment, c.Service}
}

// CollectorOptions are the user's settings for the trace collector.
type CollectorOptions struct {
	Namespace    string
	Size         resource.Quantity
	StorageClass string
	NodeSelector map[string]string
	Image        string
	PullPolicy   corev1.PullPolicy
	// Deadline, if set, is when the collector stops writing trace files.
	Deadline time.Time
	// OpenShift leaves the user and group ids to OpenShift, which assigns
	// each project its own.
	OpenShift bool
	// KServe adds the input for KServe's payload logger.
	KServe bool
}

// RenderCollector returns the trace collector objects for opts.
func RenderCollector(opts CollectorOptions) (*Collector, error) {
	objs, err := decode(kit.CollectorManifest)
	if err != nil {
		return nil, err
	}
	c := &Collector{}
	for _, o := range objs {
		switch v := o.(type) {
		case *corev1.ConfigMap:
			c.ConfigMap = v
		case *corev1.PersistentVolumeClaim:
			c.Volume = v
		case *appsv1.Deployment:
			c.Deployment = v
		case *corev1.Service:
			c.Service = v
		}
	}
	if c.ConfigMap == nil || c.Volume == nil || c.Deployment == nil || c.Service == nil {
		return nil, fmt.Errorf("collector manifest: missing object")
	}
	config, err := withBackups(c.ConfigMap.Data["config.yaml"], opts.Size)
	if err != nil {
		return nil, err
	}
	if config, err = WithDeadline(config, opts.Deadline); err != nil {
		return nil, err
	}
	if opts.KServe {
		if config, err = withKServe(config); err != nil {
			return nil, err
		}
	}
	c.ConfigMap.Data["config.yaml"] = config
	c.Volume.Spec.Resources.Requests[corev1.ResourceStorage] = opts.Size
	if opts.StorageClass != "" {
		class := opts.StorageClass
		c.Volume.Spec.StorageClassName = &class
	}
	pod := &c.Deployment.Spec.Template.Spec
	if len(opts.NodeSelector) > 0 {
		pod.NodeSelector = opts.NodeSelector
	}
	if opts.Image != "" {
		pod.Containers[0].Image = opts.Image
	}
	if opts.PullPolicy != "" {
		pod.Containers[0].ImagePullPolicy = opts.PullPolicy
	}
	if opts.OpenShift {
		withoutFixedIDs(pod)
	}
	if opts.KServe {
		pod.Containers[0].Ports = append(pod.Containers[0].Ports,
			corev1.ContainerPort{Name: "kserve", ContainerPort: KServePort})
		c.Service.Spec.Ports = append(c.Service.Spec.Ports, corev1.ServicePort{
			Name: "kserve", Port: KServePort, TargetPort: intstr.FromString("kserve"),
		})
	}
	for _, o := range c.Objects() {
		setNamespace(o, opts.Namespace)
	}
	return c, nil
}

// AnalyzerOptions are the user's settings for an analyzer Job.
type AnalyzerOptions struct {
	Namespace    string
	Image        string
	PullPolicy   corev1.PullPolicy
	AnalyzerArgs string
	Memory       resource.Quantity
	CPU          resource.Quantity
	// Volume and Platform: for the Tensormesh Platform, the Job reads the
	// platform collector's volume and runs next to the platform collector.
	Volume   string
	Platform bool
	// OpenShift: see CollectorOptions.
	OpenShift bool
}

// RenderAnalyzer returns the analyzer Job for format.
func RenderAnalyzer(format string, opts AnalyzerOptions) (*batchv1.Job, error) {
	raw, ok := kit.AnalyzerManifests[format]
	if !ok {
		return nil, fmt.Errorf("unknown trace format %q (use %s)", format, strings.Join(Formats, ", "))
	}
	objs, err := decode(raw)
	if err != nil {
		return nil, err
	}
	job, ok := objs[0].(*batchv1.Job)
	if len(objs) != 1 || !ok {
		return nil, fmt.Errorf("analyzer manifest for %s: expected one Job", format)
	}
	pod := &job.Spec.Template.Spec
	c := &pod.Containers[0]
	if opts.Image != "" {
		c.Image = opts.Image
	}
	if opts.PullPolicy != "" {
		c.ImagePullPolicy = opts.PullPolicy
	}
	for i := range c.Env {
		if c.Env[i].Name == "ANALYZER_ARGS" {
			c.Env[i].Value = opts.AnalyzerArgs
		}
	}
	setLimit(&c.Resources, corev1.ResourceMemory, opts.Memory)
	setLimit(&c.Resources, corev1.ResourceCPU, opts.CPU)
	if opts.Platform {
		for i := range pod.Volumes {
			if pod.Volumes[i].Name == "traces" {
				pod.Volumes[i].PersistentVolumeClaim.ClaimName = opts.Volume
			}
		}
		term := &pod.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
		term.LabelSelector.MatchLabels = map[string]string{PlatformCollectorLabel: PlatformCollectorValue}
	}
	if opts.OpenShift {
		withoutFixedIDs(pod)
	}
	setNamespace(job, opts.Namespace)
	return job, nil
}

// withoutFixedIDs removes the fixed user and group ids from pod. OpenShift's
// default policy rejects ids outside the project's range and assigns them
// itself; the images run with any user id.
func withoutFixedIDs(pod *corev1.PodSpec) {
	if sc := pod.SecurityContext; sc != nil {
		sc.RunAsUser, sc.RunAsGroup, sc.FSGroup = nil, nil, nil
	}
}

// DockerConfig returns the collector configuration for Docker, sized for size.
func DockerConfig(size resource.Quantity, deadline time.Time) ([]byte, error) {
	config, err := withBackups(string(kit.DockerCollectorConfig), size)
	if err != nil {
		return nil, err
	}
	config, err = WithDeadline(config, deadline)
	return []byte(config), err
}

// setLimit sets a resource limit, and lowers the request to it if needed.
func setLimit(r *corev1.ResourceRequirements, name corev1.ResourceName, q resource.Quantity) {
	if q.IsZero() {
		return
	}
	r.Limits[name] = q
	if req, ok := r.Requests[name]; ok && req.Cmp(q) > 0 {
		r.Requests[name] = q
	}
}

func withBackups(config string, size resource.Quantity) (string, error) {
	n, err := Backups(size)
	if err != nil {
		return "", err
	}
	if strings.Count(config, defaultBackups) != 1 {
		return "", fmt.Errorf("collector configuration: %q not found", defaultBackups)
	}
	return strings.Replace(config, defaultBackups, fmt.Sprintf("max_backups: %d", n), 1), nil
}

const requestPipeline = "processors: [memory_limiter, filter/request-trace, batch/request-trace]"

// WithDeadline adds a filter to the request trace pipelines that drops every
// record after deadline, so the collector stops writing trace files then. The
// files written before stay for the analysis.
func WithDeadline(config string, deadline time.Time) (string, error) {
	if deadline.IsZero() {
		return config, nil
	}
	m := regexp.MustCompile(`(?m)^( *)processors:\n`).FindStringSubmatchIndex(config)
	if m == nil || strings.Count(config, requestPipeline) != 2 {
		return "", fmt.Errorf("collector configuration: processors not found")
	}
	indent := config[m[2]:m[3]]
	cond := fmt.Sprintf("'UnixSeconds(Now()) > %d'", deadline.Unix())
	filter := strings.Join([]string{
		"  filter/deadline:",
		"    error_mode: ignore",
		"    traces:",
		"      span:",
		"        - " + cond,
		"    logs:",
		"      log_record:",
		"        - " + cond,
	}, "\n"+indent) + "\n"
	config = config[:m[1]] + indent + filter + config[m[1]:]
	return strings.ReplaceAll(config, requestPipeline,
		"processors: [memory_limiter, filter/deadline, filter/request-trace, batch/request-trace]"), nil
}

// withKServe adds the input for KServe's payload logger: a webhook receiver
// that turns each request, a CloudEvent, into a log record and keeps the ce-*
// headers. The filter drops the response events of logger mode all, and
// anything else that is not a KServe request.
func withKServe(config string) (string, error) {
	recv := regexp.MustCompile(`(?m)^( *)receivers:\n`).FindStringSubmatchIndex(config)
	logs := regexp.MustCompile(`(?m)^( *)logs:\n( *)receivers: \[otlp\]\n`).FindAllStringSubmatchIndex(config, -1)
	drop := regexp.MustCompile(`(?m)^( *)- 'instrumentation_scope.name == "dynamo.request_trace".*\n`).FindAllStringSubmatchIndex(config, -1)
	if recv == nil || len(logs) != 1 || len(drop) != 1 {
		return "", fmt.Errorf("collector configuration: receivers, logs pipeline or log filter not found")
	}
	indent := config[recv[2]:recv[3]]
	receiver := strings.Join([]string{
		"  " + KServeReceiver + ":",
		fmt.Sprintf("    endpoint: 0.0.0.0:%d", KServePort),
		"    path: " + KServePath,
		"    max_request_body_size: 67108864",
		`    header_attribute_regex: "^Ce-"`,
	}, "\n"+indent) + "\n"
	filter := config[drop[0][2]:drop[0][3]] + `- 'instrumentation_scope.name == "otlp/webhook_event" and attributes["header.Ce-Type"][0] != "org.kubeflow.serving.inference.request"'` + "\n"
	// Edit from the end, so that the earlier offsets stay valid.
	config = config[:logs[0][1]-len("[otlp]\n")] + "[otlp, " + KServeReceiver + "]\n" + config[logs[0][1]:]
	config = config[:drop[0][1]] + filter + config[drop[0][1]:]
	return config[:recv[1]] + indent + receiver + config[recv[1]:], nil
}

// HasKServe reports whether a collector configuration has the KServe input.
func HasKServe(config string) bool {
	return strings.Contains(config, KServeReceiver+":")
}

var deadlinePattern = regexp.MustCompile(`UnixSeconds\(Now\(\)\) > (\d+)`)

// Deadline returns the deadline that WithDeadline set in config, if any.
func Deadline(config string) (time.Time, bool) {
	m := deadlinePattern.FindStringSubmatch(config)
	if m == nil {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

func setNamespace(o runtime.Object, ns string) {
	type namespaced interface{ SetNamespace(string) }
	if v, ok := o.(namespaced); ok {
		v.SetNamespace(ns)
	}
}

// decode parses a multi-document YAML manifest into typed objects.
func decode(manifest []byte) ([]runtime.Object, error) {
	var objs []runtime.Object
	for _, doc := range bytes.Split(manifest, []byte("\n---\n")) {
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		o, _, err := scheme.Codecs.UniversalDeserializer().Decode(doc, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		objs = append(objs, o)
	}
	return objs, nil
}
