package kube

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

// metricsPort serves the collector's own metrics, on localhost in its pod.
const metricsPort = 8888

// Status is what the trace collector has done so far.
type Status struct {
	Pod, Node, Phase string
	Started          time.Time
	VolumeSize       string
	Deadline         time.Time
	// Counts are the collector's own counters since it started, from its
	// metrics, or nil if they could not be read.
	Counts *Counts
	// CountsError is why Counts is nil.
	CountsError error
}

// Counts are the request records the collector wrote or dropped.
type Counts struct {
	// Spans and LogRecords were written to the trace files: LMCache requests
	// are spans, NVIDIA Dynamo requests are log records.
	Spans, LogRecords float64
	// AfterDeadline were dropped after the --duration deadline.
	AfterDeadline float64
}

// CollectorStatus reports the state of the collector of t, and how many
// records it wrote, read from its metrics through a port forward.
func (c *Client) CollectorStatus(ctx context.Context, t *Target) (*Status, error) {
	selector := "app.kubernetes.io/name=" + Collector
	if t.Kind == Platform {
		selector = manifest.PlatformCollectorLabel + "=" + manifest.PlatformCollectorValue
	}
	s := &Status{}
	if pvc, err := c.K8s.CoreV1().PersistentVolumeClaims(t.Namespace).Get(ctx, t.Volume, metav1.GetOptions{}); err == nil {
		if q, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
			s.VolumeSize = q.String()
		} else if q, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			s.VolumeSize = q.String()
		}
	}
	if t.Kind == Kit {
		if cm, err := c.K8s.CoreV1().ConfigMaps(t.Namespace).Get(ctx, Collector, metav1.GetOptions{}); err == nil {
			s.Deadline, _ = manifest.Deadline(cm.Data["config.yaml"])
		}
	}
	pods, err := c.K8s.CoreV1().Pods(t.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp == nil && (pod == nil || p.Status.Phase == corev1.PodRunning) {
			pod = p
		}
	}
	if pod == nil {
		s.CountsError = errors.New("no collector pod")
		return s, nil
	}
	s.Pod, s.Node, s.Phase = pod.Name, pod.Spec.NodeName, string(pod.Status.Phase)
	if pod.Status.StartTime != nil {
		s.Started = pod.Status.StartTime.Time
	}
	if pod.Status.Phase != corev1.PodRunning {
		s.CountsError = fmt.Errorf("the collector pod is %s", pod.Status.Phase)
		return s, nil
	}
	text, err := c.podMetrics(ctx, t.Namespace, pod.Name)
	if err != nil {
		s.CountsError = err
		return s, nil
	}
	s.Counts = ParseCounts(text)
	return s, nil
}

// podMetrics reads the collector's metrics through a port forward, because
// they are served on localhost in the pod only.
func (c *Client) podMetrics(ctx context.Context, ns, pod string) (string, error) {
	if c.Config == nil {
		return "", errors.New("no cluster connection")
	}
	transport, upgrader, err := spdy.RoundTripperFor(c.Config)
	if err != nil {
		return "", err
	}
	url := c.K8s.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
	stop, ready := make(chan struct{}), make(chan struct{})
	defer close(stop)
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", metricsPort)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", err
	}
	failed := make(chan error, 1)
	go func() { failed <- fw.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-failed:
		return "", fmt.Errorf("port forward to %s: %w", pod, err)
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("port forward to %s timed out", pod)
	case <-ctx.Done():
		return "", ctx.Err()
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) == 0 {
		return "", fmt.Errorf("port forward to %s: %v", pod, err)
	}
	local := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := local.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", ports[0].Local))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("collector metrics: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// ParseCounts reads the request record counters from the collector's
// metrics in the Prometheus text format.
func ParseCounts(text string) *Counts {
	c := &Counts{}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ' ')
		if i < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[i+1:], 64)
		if err != nil {
			continue
		}
		name := line[:i]
		switch {
		case metric(name, "otelcol_exporter_sent_spans", `exporter="file/request-trace"`):
			c.Spans += v
		case metric(name, "otelcol_exporter_sent_log_records", `exporter="file/request-trace"`):
			c.LogRecords += v
		case metric(name, "otelcol_processor_filter_spans_filtered", `filter="filter/deadline"`),
			metric(name, "otelcol_processor_filter_logs_filtered", `filter="filter/deadline"`):
			c.AfterDeadline += v
		}
	}
	return c
}

// metric reports whether a sample name, such as name_total{a="b"}, is metric
// with the given label.
func metric(sample, name, label string) bool {
	base, labels, _ := strings.Cut(sample, "{")
	base = strings.TrimSuffix(base, "_total")
	return base == name && strings.Contains(labels, label)
}
