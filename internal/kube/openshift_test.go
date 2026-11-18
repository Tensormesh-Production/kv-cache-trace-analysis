package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestIsOpenShift(t *testing.T) {
	for _, tc := range []struct {
		groupVersions []string
		want          bool
	}{
		{[]string{"v1", "apps/v1"}, false},
		{[]string{"v1", "apps/v1", "security.openshift.io/v1"}, true},
	} {
		cs := fake.NewClientset()
		for _, gv := range tc.groupVersions {
			cs.Resources = append(cs.Resources, &metav1.APIResourceList{GroupVersion: gv})
		}
		got, err := (&Client{K8s: cs}).IsOpenShift()
		if err != nil || got != tc.want {
			t.Errorf("IsOpenShift with %v = %v, %v; want %v", tc.groupVersions, got, err, tc.want)
		}
	}
}

// Without permission to change volumes, as for a project user on OpenShift,
// stop goes on and names the volume.
func TestDeleteVolumeWithClaim(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: Volume, Namespace: testNS},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-1"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain},
	}
	for _, forbidden := range []bool{false, true} {
		cs := fake.NewClientset(pvc, pv)
		if forbidden {
			cs.PrependReactor("patch", "persistentvolumes", func(ktesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "pv-1", errors.New("no permission"))
			})
		}
		var out bytes.Buffer
		c := &Client{K8s: cs, Out: &out}
		got, err := c.DeleteVolumeWithClaim(context.Background(), testNS, Volume)
		if err != nil {
			t.Fatalf("forbidden=%v: %v", forbidden, err)
		}
		v, _ := cs.CoreV1().PersistentVolumes().Get(context.Background(), "pv-1", metav1.GetOptions{})
		policy := v.Spec.PersistentVolumeReclaimPolicy
		if !forbidden && (got != "pv-1" || policy != corev1.PersistentVolumeReclaimDelete || out.Len() > 0) {
			t.Errorf("with permission: volume %q, policy %s, output %q", got, policy, out.String())
		}
		if forbidden && (got != "" || !strings.Contains(out.String(), "No permission to change volume pv-1.")) {
			t.Errorf("without permission: volume %q, output %q", got, out.String())
		}
	}
}
