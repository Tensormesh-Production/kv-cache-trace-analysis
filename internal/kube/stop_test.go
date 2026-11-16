package kube

import (
	"context"
	"io"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

const testNS = "apps"

func namespace(labels map[string]string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, Labels: labels}}
}

func pod(name, app string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS, Labels: map[string]string{"app.kubernetes.io/name": app}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func claim(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNS}}
}

func TestEnsureNamespace(t *testing.T) {
	ctx := context.Background()
	c := &Client{K8s: fake.NewClientset(), Out: io.Discard}
	if err := c.EnsureNamespace(ctx, testNS); err != nil {
		t.Fatal(err)
	}
	n, err := c.K8s.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	if err != nil || n.Labels[ManagedByLabel] != FieldManager {
		t.Fatalf("new namespace labels = %v, %v; want %s=%s", n.Labels, err, ManagedByLabel, FieldManager)
	}

	c = &Client{K8s: fake.NewClientset(namespace(nil)), Out: io.Discard}
	if err := c.EnsureNamespace(ctx, testNS); err != nil {
		t.Fatal(err)
	}
	n, _ = c.K8s.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{})
	if _, ok := n.Labels[ManagedByLabel]; ok {
		t.Fatalf("an existing namespace got the label %s", ManagedByLabel)
	}
}

func TestOwnsNamespace(t *testing.T) {
	ours := map[string]string{ManagedByLabel: FieldManager}
	for _, tc := range []struct {
		name string
		objs []runtime.Object
		want bool
	}{
		{"created by tm-trace", []runtime.Object{namespace(ours),
			pod("collector", Collector, corev1.PodRunning), pod("analyzer", analyzerName, corev1.PodSucceeded),
			pod("loader", "trace-loader", corev1.PodSucceeded), claim(Volume)}, true},
		{"existed before start", []runtime.Object{namespace(nil),
			pod("collector", Collector, corev1.PodRunning), claim(Volume)}, false},
		{"another app runs in it", []runtime.Object{namespace(ours),
			pod("collector", Collector, corev1.PodRunning), pod("app", "app", corev1.PodRunning)}, false},
		{"another app's volume", []runtime.Object{namespace(ours), claim(Volume), claim("app-data")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{K8s: fake.NewClientset(tc.objs...), Out: io.Discard}
			got, err := c.OwnsNamespace(context.Background(), testNS)
			if err != nil || got != tc.want {
				t.Fatalf("OwnsNamespace = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestDeleteCollector(t *testing.T) {
	ctx := context.Background()
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: testNS} }
	c := &Client{K8s: fake.NewClientset(
		namespace(nil),
		&appsv1.Deployment{ObjectMeta: meta(Collector)},
		&corev1.Service{ObjectMeta: meta(Collector)},
		&corev1.ConfigMap{ObjectMeta: meta(Collector)},
		claim(Volume),
		&appsv1.Deployment{ObjectMeta: meta("app")},
		&corev1.ConfigMap{ObjectMeta: meta("app-config")},
		claim("app-data"),
	), Out: io.Discard}
	if err := c.DeleteCollector(ctx, testNS, "", time.Minute); err != nil {
		t.Fatal(err)
	}
	gone := map[string]error{}
	_, gone["deployment"] = c.K8s.AppsV1().Deployments(testNS).Get(ctx, Collector, metav1.GetOptions{})
	_, gone["service"] = c.K8s.CoreV1().Services(testNS).Get(ctx, Collector, metav1.GetOptions{})
	_, gone["configmap"] = c.K8s.CoreV1().ConfigMaps(testNS).Get(ctx, Collector, metav1.GetOptions{})
	_, gone["volume claim"] = c.K8s.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, Volume, metav1.GetOptions{})
	for kind, err := range gone {
		if !apierrors.IsNotFound(err) {
			t.Errorf("the collector's %s is still there (%v)", kind, err)
		}
	}
	if _, err := c.K8s.AppsV1().Deployments(testNS).Get(ctx, "app", metav1.GetOptions{}); err != nil {
		t.Errorf("another app's deployment was deleted: %v", err)
	}
	if _, err := c.K8s.CoreV1().ConfigMaps(testNS).Get(ctx, "app-config", metav1.GetOptions{}); err != nil {
		t.Errorf("another app's configmap was deleted: %v", err)
	}
	if _, err := c.K8s.CoreV1().PersistentVolumeClaims(testNS).Get(ctx, "app-data", metav1.GetOptions{}); err != nil {
		t.Errorf("another app's volume claim was deleted: %v", err)
	}
	if _, err := c.K8s.CoreV1().Namespaces().Get(ctx, testNS, metav1.GetOptions{}); err != nil {
		t.Errorf("the namespace was deleted: %v", err)
	}
}
