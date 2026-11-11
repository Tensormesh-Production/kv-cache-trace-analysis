// Package kube runs the collector and the analyzer on Kubernetes.
package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// FieldManager names tm-trace as the owner of the fields it applies.
const FieldManager = "tm-trace"

// Client talks to one cluster.
type Client struct {
	K8s     kubernetes.Interface
	Dynamic dynamic.Interface
	Out     io.Writer
	// Config is the connection, for port forwards.
	Config *rest.Config
}

// New connects with the user's kubeconfig, like kubectl: the given file or
// KUBECONFIG or ~/.kube/config, and the given context or the current one.
func New(kubeconfig, kubeContext string, out io.Writer) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if clientcmd.IsEmptyConfig(err) {
		return nil, errors.New("no kubeconfig found: set KUBECONFIG or --kubeconfig, or add --docker to run without Kubernetes")
	}
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cfg.UserAgent = "tm-trace"
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		return nil, fmt.Errorf("cannot reach the cluster (%s): %w", cfg.Host, err)
	}
	return &Client{K8s: cs, Dynamic: dyn, Out: out, Config: cfg}, nil
}

func (c *Client) say(format string, args ...any) {
	fmt.Fprintf(c.Out, format+"\n", args...)
}

// EnsureNamespace creates the namespace if it does not exist.
func (c *Client) EnsureNamespace(ctx context.Context, ns string) error {
	_, err := c.K8s.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = c.K8s.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{FieldManager: FieldManager})
	if err == nil {
		c.say("Created namespace %s", ns)
	}
	return err
}

// Apply creates or updates an object with server-side apply. A volume claim
// that already exists is left as it is, because most of its fields cannot change.
func (c *Client) Apply(ctx context.Context, obj runtime.Object) error {
	gvks, _, err := scheme.Scheme.ObjectKinds(obj)
	if err != nil {
		return err
	}
	obj.GetObjectKind().SetGroupVersionKind(gvks[0])
	meta, err := metaOf(obj)
	if err != nil {
		return err
	}
	ns, name := meta.GetNamespace(), meta.GetName()
	opts := metav1.PatchOptions{FieldManager: FieldManager, Force: ptr(true)}
	switch o := obj.(type) {
	case *corev1.PersistentVolumeClaim:
		existing, err := c.K8s.CoreV1().PersistentVolumeClaims(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			want := o.Spec.Resources.Requests[corev1.ResourceStorage]
			have := existing.Spec.Resources.Requests[corev1.ResourceStorage]
			if want.Cmp(have) != 0 {
				c.say("Volume %s already exists with size %s; keeping it", name, have.String())
			}
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		_, err = c.K8s.CoreV1().PersistentVolumeClaims(ns).Create(ctx, o, metav1.CreateOptions{FieldManager: FieldManager})
		return err
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	switch obj.(type) {
	case *corev1.ConfigMap:
		_, err = c.K8s.CoreV1().ConfigMaps(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
	case *corev1.Service:
		_, err = c.K8s.CoreV1().Services(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
	case *appsv1.Deployment:
		_, err = c.K8s.AppsV1().Deployments(ns).Patch(ctx, name, types.ApplyPatchType, data, opts)
	default:
		return fmt.Errorf("apply: unsupported object %T", obj)
	}
	return err
}

// WaitDeployment waits until the deployment has an available, updated pod.
func (c *Client) WaitDeployment(ctx context.Context, ns, name string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastNote string
	for {
		d, err := c.K8s.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && d.Status.ObservedGeneration >= d.Generation &&
			d.Status.UpdatedReplicas >= 1 && d.Status.AvailableReplicas >= 1 && d.Status.UnavailableReplicas == 0 {
			return nil
		}
		if note := c.podProblem(ctx, ns, "app.kubernetes.io/name="+name); note != "" && note != lastNote {
			c.say("  %s", note)
			lastNote = note
		}
		select {
		case <-ctx.Done():
			if lastNote != "" {
				return fmt.Errorf("deployment %s is not ready after %s: %s", name, timeout, lastNote)
			}
			return fmt.Errorf("deployment %s is not ready after %s", name, timeout)
		case <-time.After(2 * time.Second):
		}
	}
}

// podProblem describes why a pod matching selector is not running yet, if known.
func (c *Client) podProblem(ctx context.Context, ns, selector string) string {
	pods, err := c.K8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	p := pods.Items[0]
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
			return fmt.Sprintf("pod %s: %s: %s", p.Name, w.Reason, w.Message)
		}
	}
	if p.Status.Phase == corev1.PodPending {
		for _, cond := range p.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Message != "" {
				return fmt.Sprintf("pod %s is waiting for a node: %s", p.Name, cond.Message)
			}
		}
	}
	return ""
}

func metaOf(obj runtime.Object) (metav1.Object, error) {
	m, ok := obj.(metav1.Object)
	if !ok {
		return nil, fmt.Errorf("object %T has no metadata", obj)
	}
	return m, nil
}

func ptr[T any](v T) *T { return &v }
