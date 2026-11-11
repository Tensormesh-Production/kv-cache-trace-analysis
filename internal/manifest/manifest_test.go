package manifest

import (
	"bytes"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"

	kit "github.com/tensormesh-production/kv-cache-trace-analysis"
)

func TestBackups(t *testing.T) {
	for size, want := range map[string]int{"50Gi": 80, "10Gi": 16, "5Gi": 8, "100Gi": 160, "7680Mi": 12} {
		got, err := Backups(resource.MustParse(size))
		if err != nil || got != want {
			t.Errorf("Backups(%s) = %d, %v; want %d", size, got, err, want)
		}
	}
	if _, err := Backups(resource.MustParse("4Gi")); err == nil {
		t.Error("Backups(4Gi): want an error below the minimum")
	}
	usage, _ := MaxUsage(resource.MustParse("50Gi"))
	if usage != 81*512<<20 {
		t.Errorf("MaxUsage(50Gi) = %d", usage)
	}
	// --max-size: the trace files always stay under it.
	for _, size := range []string{"5Gi", "5500Mi", "7680Mi", "10Gi", "50Gi", "1Ti"} {
		q := resource.MustParse(size)
		if usage, err := MaxUsage(q); err != nil || usage >= q.Value() {
			t.Errorf("MaxUsage(%s) = %d, %v; want below %d", size, usage, err, q.Value())
		}
	}
}

// With default options, the rendered collector equals deploy/collector/trace-collector.yaml.
func TestRenderCollectorDefaults(t *testing.T) {
	got, err := RenderCollector(CollectorOptions{Namespace: "trace-analysis", Size: resource.MustParse(DefaultSize)})
	if err != nil {
		t.Fatal(err)
	}
	want, err := decode(kit.CollectorManifest)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range got.Objects() {
		setNamespace(want[i], "trace-analysis")
		if !equality.Semantic.DeepEqual(o, want[i]) {
			t.Errorf("object %d (%T) differs from the manifest", i, o)
		}
	}
}

func TestRenderCollectorOptions(t *testing.T) {
	c, err := RenderCollector(CollectorOptions{
		Namespace: "ns", Size: resource.MustParse("10Gi"), StorageClass: "fast",
		NodeSelector: map[string]string{"kubernetes.io/hostname": "node-1"}, Image: "mirror/otelcol:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(c.ConfigMap.Data["config.yaml"], "max_backups: 16\n") {
		t.Error("rotation not sized for 10Gi")
	}
	if q := c.Volume.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "10Gi" {
		t.Errorf("volume size %s", q.String())
	}
	if c.Volume.Spec.StorageClassName == nil || *c.Volume.Spec.StorageClassName != "fast" {
		t.Error("storage class not set")
	}
	pod := c.Deployment.Spec.Template.Spec
	if pod.NodeSelector["kubernetes.io/hostname"] != "node-1" || pod.Containers[0].Image != "mirror/otelcol:1" {
		t.Error("node selector or image not set")
	}
	for _, o := range c.Objects() {
		if o.(interface{ GetNamespace() string }).GetNamespace() != "ns" {
			t.Errorf("%T: namespace not set", o)
		}
	}
}

// With default options, each rendered Job equals its file in deploy/analyzer.
func TestRenderAnalyzerDefaults(t *testing.T) {
	for _, format := range Formats {
		got, err := RenderAnalyzer(format, AnalyzerOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := decode(kit.AnalyzerManifests[format])
		if !equality.Semantic.DeepEqual(got, want[0]) {
			t.Errorf("%s: Job differs from the manifest", format)
		}
	}
	if _, err := RenderAnalyzer("other", AnalyzerOptions{}); err == nil {
		t.Error("unknown format: want an error")
	}
}

func TestRenderAnalyzerPlatform(t *testing.T) {
	job, err := RenderAnalyzer("lmcache", AnalyzerOptions{
		Namespace: "tensormesh-platform", Image: "mirror/analyzer:0.5.7", PullPolicy: corev1.PullIfNotPresent,
		AnalyzerArgs: "--model m", Memory: resource.MustParse("512Mi"), CPU: resource.MustParse("4"),
		Volume: "tensormesh-platform-otel-collector-request-trace", Platform: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]
	if job.Namespace != "tensormesh-platform" || c.Image != "mirror/analyzer:0.5.7" || c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Error("namespace, image or pull policy not set")
	}
	if c.Env[1].Name != "ANALYZER_ARGS" || c.Env[1].Value != "--model m" {
		t.Errorf("env %v", c.Env)
	}
	if m := c.Resources.Limits[corev1.ResourceMemory]; m.String() != "512Mi" {
		t.Errorf("memory limit %s", m.String())
	}
	if m := c.Resources.Requests[corev1.ResourceMemory]; m.String() != "512Mi" {
		t.Errorf("memory request %s, want lowered to the limit", m.String())
	}
	if pod.Volumes[0].PersistentVolumeClaim.ClaimName != "tensormesh-platform-otel-collector-request-trace" {
		t.Error("claim not set")
	}
	sel := pod.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector.MatchLabels
	if len(sel) != 1 || sel[PlatformCollectorLabel] != PlatformCollectorValue {
		t.Errorf("affinity %v", sel)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("ServiceAccount token must stay off")
	}
}

func TestDockerConfig(t *testing.T) {
	def, err := DockerConfig(resource.MustParse(DefaultSize))
	if err != nil || !bytes.Equal(def, kit.DockerCollectorConfig) {
		t.Error("default Docker config differs from deploy/docker/collector-config.yaml")
	}
	small, _ := DockerConfig(resource.MustParse("5Gi"))
	if !bytes.Contains(small, []byte("max_backups: 8\n")) {
		t.Error("rotation not sized for 5Gi")
	}
}
