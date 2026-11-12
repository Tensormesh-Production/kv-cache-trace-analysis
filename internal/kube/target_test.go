package kube

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestHelmCommand(t *testing.T) {
	r := &Release{Name: "tensormesh-platform", Version: "1.0.1", Namespace: "tensormesh-platform"}
	want := "helm upgrade tensormesh-platform oci://artifacts.tensormesh.ai/tensormesh-production/charts/tensormesh-platform --version 1.0.1 -n tensormesh-platform --reuse-values -f trace-values.yaml"
	if got := HelmCommand(r, "trace-values.yaml"); got != want {
		t.Errorf("HelmCommand:\n%s", got)
	}
}

func TestOlderThan057(t *testing.T) {
	for tag, want := range map[string]bool{
		"v0.5.6": true, "0.4.2": true, "v0.5.6-amd64": true, "v0.5.7": false, "v0.5.7rc4": false,
		"v0.5.7-amd64": false, "v0.5.7-lightweight-arm64": false, "v0.6.0": false,
		"v1.0.0": false, "latest": false, "": false, "nightly-2026": false,
	} {
		if got := olderThan057(tag); got != want {
			t.Errorf("olderThan057(%q) = %v", tag, got)
		}
	}
}

func TestFinished(t *testing.T) {
	cond := func(ty batchv1.JobConditionType) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: ty, Status: corev1.ConditionTrue}}}}
	}
	for _, c := range []struct {
		job      *batchv1.Job
		done, ok bool
	}{
		{&batchv1.Job{}, false, false},
		{cond(batchv1.JobComplete), true, true},
		{cond(batchv1.JobSuccessCriteriaMet), true, true},
		{cond(batchv1.JobFailed), true, false},
		{cond(batchv1.JobFailureTarget), true, false},
		{cond(batchv1.JobSuspended), false, false},
	} {
		if done, ok := finished(c.job); done != c.done || ok != c.ok {
			t.Errorf("%v: done %v ok %v", c.job.Status.Conditions, done, ok)
		}
	}
}
