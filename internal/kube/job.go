package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ErrOutOfMemory: the analyzer ran out of memory.
var ErrOutOfMemory = errors.New("the analyzer ran out of memory")

// JobError is a failed analyzer Job, with its log.
type JobError struct {
	Log []byte
}

func (e *JobError) Error() string { return "the analyzer failed" }

// failFast are pod states that will not recover without a change by the user.
var failFast = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
}

// RunJob runs an analyzer Job to the end and returns its log, which is the
// HTML report when the Job succeeds. The Job is always deleted afterwards.
func (c *Client) RunJob(ctx context.Context, job *batchv1.Job, timeout time.Duration) ([]byte, error) {
	jobs := c.K8s.BatchV1().Jobs(job.Namespace)
	if err := c.deleteJob(ctx, job.Namespace, job.Name, true); err != nil {
		return nil, err
	}
	if _, err := jobs.Create(ctx, job, metav1.CreateOptions{FieldManager: FieldManager}); err != nil {
		return nil, fmt.Errorf("create job %s: %w", job.Name, err)
	}
	defer func() {
		// Use a fresh context: clean up even if ctx was cancelled.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = c.deleteJob(cleanup, job.Namespace, job.Name, false)
	}()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	selector := "job-name=" + job.Name
	var lastNote string
	for {
		j, err := jobs.Get(ctx, job.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if done, ok := finished(j); done {
			pod, err := c.jobPod(ctx, job.Namespace, selector)
			if err != nil {
				return nil, err
			}
			log, err := c.podLog(ctx, job.Namespace, pod.Name)
			if ok {
				return log, err
			}
			if oomKilled(pod) {
				return log, ErrOutOfMemory
			}
			return log, &JobError{Log: log}
		}
		if pod, err := c.jobPod(ctx, job.Namespace, selector); err == nil {
			for _, s := range pod.Status.ContainerStatuses {
				if w := s.State.Waiting; w != nil && failFast[w.Reason] {
					return nil, fmt.Errorf("analyzer pod %s: %s: %s", pod.Name, w.Reason, w.Message)
				}
			}
		}
		if note := c.podProblem(ctx, job.Namespace, selector); note != "" && note != lastNote {
			c.say("  %s", note)
			lastNote = note
		}
		select {
		case <-ctx.Done():
			msg := fmt.Sprintf("the analyzer did not finish within %s", timeout)
			if lastNote != "" {
				msg += ": " + lastNote
			}
			return nil, errors.New(msg)
		case <-time.After(2 * time.Second):
		}
	}
}

// finished reports whether the Job ended, and whether it succeeded.
func finished(j *batchv1.Job) (done, ok bool) {
	for _, cond := range j.Status.Conditions {
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			return true, true
		case batchv1.JobFailed, batchv1.JobFailureTarget:
			return true, false
		}
	}
	return false, false
}

func (c *Client) jobPod(ctx context.Context, ns, selector string) (*corev1.Pod, error) {
	pods, err := c.K8s.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod for %s", selector)
	}
	return &pods.Items[0], nil
}

func (c *Client) podLog(ctx context.Context, ns, pod string) ([]byte, error) {
	r, err := c.K8s.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: "analyzer"}).Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the analyzer log: %w", err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

func oomKilled(p *corev1.Pod) bool {
	for _, s := range p.Status.ContainerStatuses {
		if t := s.State.Terminated; t != nil && t.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

// deleteJob deletes a Job and its pod. With wait, it returns once both are gone.
func (c *Client) deleteJob(ctx context.Context, ns, name string, wait bool) error {
	policy := metav1.DeletePropagationForeground
	err := c.K8s.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil || !wait {
		return err
	}
	for i := 0; i < 60; i++ {
		_, err := c.K8s.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("job %s is still being deleted", name)
}
