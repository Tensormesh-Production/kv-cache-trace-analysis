package kube

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// FormatConfigMap holds a custom trace format script.
	FormatConfigMap = "trace-format"
	// ManagedByLabel, set to FieldManager, marks a namespace that tm-trace
	// created. stop deletes only such a namespace.
	ManagedByLabel = "app.kubernetes.io/managed-by"
	analyzerName   = "trace-analyzer"
)

// OwnsNamespace reports whether stop can delete all of ns: tm-trace created
// it, and no pods or volume claims of other apps are in it.
func (c *Client) OwnsNamespace(ctx context.Context, ns string) (bool, error) {
	n, err := c.K8s.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	if n.Labels[ManagedByLabel] != FieldManager {
		return false, nil
	}
	pods, err := c.K8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		name := p.Labels["app.kubernetes.io/name"]
		done := p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
		if name != Collector && name != analyzerName && !done {
			return false, nil
		}
	}
	claims, err := c.K8s.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, err
	}
	for _, claim := range claims.Items {
		if claim.Name != Volume {
			return false, nil
		}
	}
	return true, nil
}

// DeleteCollector deletes the trace collector and its volume claim in ns,
// and waits until the claim and the volume pv are gone. Other objects in ns
// stay.
func (c *Client) DeleteCollector(ctx context.Context, ns, pv string, timeout time.Duration) error {
	policy := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	deletes := []func() error{
		func() error { return c.K8s.AppsV1().Deployments(ns).Delete(ctx, Collector, opts) },
		func() error { return c.K8s.CoreV1().Services(ns).Delete(ctx, Collector, opts) },
		func() error { return c.K8s.CoreV1().ConfigMaps(ns).Delete(ctx, Collector, opts) },
		func() error { return c.K8s.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, Volume, opts) },
	}
	for _, del := range deletes {
		if err := del(); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete the trace collector: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, err := c.K8s.CoreV1().PersistentVolumeClaims(ns).Get(ctx, Volume, metav1.GetOptions{})
		gone := apierrors.IsNotFound(err)
		if pv != "" {
			v, err := c.K8s.CoreV1().PersistentVolumes().Get(ctx, pv, metav1.GetOptions{})
			if err == nil && v.Status.Phase == corev1.VolumeFailed {
				return fmt.Errorf("volume %s could not be deleted: %s; check it with: kubectl get pv %s", pv, v.Status.Message, pv)
			}
			gone = gone && apierrors.IsNotFound(err)
		}
		if gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the trace volume is still being deleted after %s; check it with: kubectl -n %s get pvc %s", timeout, ns, Volume)
		case <-time.After(2 * time.Second):
		}
	}
}

// DeleteAnalyzers deletes the analyzer Jobs and the custom format ConfigMap in ns.
func (c *Client) DeleteAnalyzers(ctx context.Context, ns string) error {
	policy := metav1.DeletePropagationBackground
	err := c.K8s.BatchV1().Jobs(ns).DeleteCollection(ctx,
		metav1.DeleteOptions{PropagationPolicy: &policy},
		metav1.ListOptions{LabelSelector: "app.kubernetes.io/name=trace-analyzer"})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete analyzer jobs: %w", err)
	}
	err = c.K8s.CoreV1().ConfigMaps(ns).Delete(ctx, FormatConfigMap, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteVolumeWithClaim makes the volume bound to claim deletable with it:
// with a Retain storage class, the data would stay after the claim is gone.
// It returns the volume name, or "" if the claim is not bound.
func (c *Client) DeleteVolumeWithClaim(ctx context.Context, ns, claim string) (string, error) {
	pvc, err := c.K8s.CoreV1().PersistentVolumeClaims(ns).Get(ctx, claim, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	pv := pvc.Spec.VolumeName
	if pv == "" {
		return "", nil
	}
	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
	_, err = c.K8s.CoreV1().PersistentVolumes().Patch(ctx, pv, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return "", fmt.Errorf("set volume %s to be deleted with its claim: %w", pv, err)
	}
	return pv, nil
}

// DeleteNamespace deletes ns and waits until it and the volume pv are gone.
func (c *Client) DeleteNamespace(ctx context.Context, ns, pv string, timeout time.Duration) error {
	err := c.K8s.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, nsErr := c.K8s.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		pvGone := true
		if pv != "" {
			v, err := c.K8s.CoreV1().PersistentVolumes().Get(ctx, pv, metav1.GetOptions{})
			pvGone = apierrors.IsNotFound(err)
			if err == nil && v.Status.Phase == corev1.VolumeFailed {
				return fmt.Errorf("volume %s could not be deleted: %s; check it with: kubectl get pv %s", pv, v.Status.Message, pv)
			}
		}
		if apierrors.IsNotFound(nsErr) && pvGone {
			return nil
		}
		select {
		case <-ctx.Done():
			if !apierrors.IsNotFound(nsErr) {
				return fmt.Errorf("namespace %s is still being deleted after %s", ns, timeout)
			}
			return fmt.Errorf("volume %s is still being deleted after %s; check it with: kubectl get pv %s", pv, timeout, pv)
		case <-time.After(2 * time.Second):
		}
	}
}
