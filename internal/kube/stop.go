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

// FormatConfigMap holds a custom trace format script.
const FormatConfigMap = "trace-format"

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
