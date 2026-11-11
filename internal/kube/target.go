package kube

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tensormesh-production/kv-cache-trace-analysis/internal/manifest"
)

const (
	// Collector, Volume: the trace collector deployed by tm-trace start.
	Collector = "trace-collector"
	Volume    = "trace-collector-data"
	// PlatformNamespace is where the Tensormesh Platform is installed by default.
	PlatformNamespace = "tensormesh-platform"
	// PlatformChart is the Helm chart of the Tensormesh Platform.
	PlatformChart = "oci://artifacts.tensormesh.ai/tensormesh-production/charts/tensormesh-platform"
	// TokenTraceFlag turns on LMCache request traces.
	TokenTraceFlag = "--enable-token-trace"
)

// Kind of trace collector found in a namespace.
type Kind int

const (
	// None: no trace collector.
	None Kind = iota
	// Kit: the trace collector from tm-trace start.
	Kit
	// Platform: the Tensormesh Platform's collector with request traces on.
	Platform
)

// Target is the trace collector to analyze.
type Target struct {
	Kind      Kind
	Namespace string
	// Volume is the claim that holds the trace files.
	Volume string
}

// Release is a Tensormesh Platform Helm release.
type Release struct {
	Name, Version, Namespace string
}

var lmcacheEngines = schema.GroupVersionResource{Group: "lmcache.lmcache.ai", Version: "v1alpha1", Resource: "lmcacheengines"}

// FindTarget looks for a trace collector in ns.
func (c *Client) FindTarget(ctx context.Context, ns string) (*Target, error) {
	_, err := c.K8s.AppsV1().Deployments(ns).Get(ctx, Collector, metav1.GetOptions{})
	if err == nil {
		return &Target{Kind: Kit, Namespace: ns, Volume: Volume}, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	if v, err := c.RequestTraceVolume(ctx, ns); err != nil || v != "" {
		return &Target{Kind: Platform, Namespace: ns, Volume: v}, err
	}
	return &Target{Kind: None, Namespace: ns}, nil
}

// RequestTraceVolume returns the platform collector's trace volume claim in ns, if any.
func (c *Client) RequestTraceVolume(ctx context.Context, ns string) (string, error) {
	pvcs, err := c.K8s.CoreV1().PersistentVolumeClaims(ns).List(ctx,
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=otel-collector"})
	if err != nil {
		return "", err
	}
	for _, p := range pvcs.Items {
		if strings.HasSuffix(p.Name, "-request-trace") {
			return p.Name, nil
		}
	}
	return "", nil
}

// PlatformCollectorRunning reports whether the platform collector pod runs in ns.
func (c *Client) PlatformCollectorRunning(ctx context.Context, ns string) (bool, error) {
	pods, err := c.K8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: manifest.PlatformCollectorLabel + "=" + manifest.PlatformCollectorValue,
		FieldSelector: "status.phase=Running",
	})
	if err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}

// FindRelease finds the Tensormesh Platform Helm release in ns from the
// chart's labels on its deployments and services. No secrets are read.
func (c *Client) FindRelease(ctx context.Context, ns string) (*Release, error) {
	sel := metav1.ListOptions{LabelSelector: "app.kubernetes.io/managed-by=Helm"}
	var labelSets []map[string]string
	deps, err := c.K8s.AppsV1().Deployments(ns).List(ctx, sel)
	if err != nil {
		return nil, err
	}
	for _, d := range deps.Items {
		labelSets = append(labelSets, d.Labels)
	}
	svcs, err := c.K8s.CoreV1().Services(ns).List(ctx, sel)
	if err != nil {
		return nil, err
	}
	for _, s := range svcs.Items {
		labelSets = append(labelSets, s.Labels)
	}
	for _, l := range labelSets {
		chart := l["helm.sh/chart"]
		if v, ok := strings.CutPrefix(chart, "tensormesh-platform-"); ok && l["app.kubernetes.io/instance"] != "" {
			return &Release{Name: l["app.kubernetes.io/instance"], Version: v, Namespace: ns}, nil
		}
	}
	return nil, nil
}

// Engine is an LMCacheEngine and whether it records request traces.
type Engine struct {
	Name       string
	TokenTrace bool
	// Old is set when the engine image is older than LMCache 0.5.7, which
	// has no request traces.
	Old   bool
	Image string
}

// Engines lists the LMCacheEngines in ns. It returns nil if the resource type
// does not exist in the cluster.
func (c *Client) Engines(ctx context.Context, ns string) ([]Engine, error) {
	list, err := c.Dynamic.Resource(lmcacheEngines).Namespace(ns).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Engine
	for _, item := range list.Items {
		spec, _ := item.Object["spec"].(map[string]any)
		e := Engine{Name: item.GetName()}
		if args, ok := spec["extraArgs"].([]any); ok {
			for _, a := range args {
				if s, _ := a.(string); s == TokenTraceFlag {
					e.TokenTrace = true
				}
			}
		}
		if img, ok := spec["image"].(map[string]any); ok {
			repo, _ := img["repository"].(string)
			tag, _ := img["tag"].(string)
			e.Image = strings.Trim(repo+":"+tag, ":")
			e.Old = olderThan057(tag)
		}
		out = append(out, e)
	}
	return out, nil
}

var semverTag = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// olderThan057 reports whether an image tag is a version below 0.5.7.
// Tags that are not versions (such as latest or a digest) are not flagged.
func olderThan057(tag string) bool {
	m := semverTag.FindStringSubmatch(tag)
	if m == nil {
		return false
	}
	v := [3]int{}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v[0] == 0 && (v[1] < 5 || v[1] == 5 && v[2] < 7)
}

// HelmCommand is the helm upgrade that applies valuesFile to the platform
// release, such as the settings that turn its request traces on or off. It
// keeps all other values of the release.
func HelmCommand(r *Release, valuesFile string) string {
	return fmt.Sprintf("helm upgrade %s %s --version %s -n %s --reuse-values -f %s",
		r.Name, PlatformChart, r.Version, r.Namespace, valuesFile)
}
