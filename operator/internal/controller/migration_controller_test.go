package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	return s
}

func newTestDeployment(name, namespace, image string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "test-uid-123",
			Labels: map[string]string{
				LabelPartOf:    LabelPartOfValue,
				LabelComponent: LabelComponentValue,
			},
			Annotations: map[string]string{
				AnnotationMigrationEnabled: "true",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "openfga"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "openfga"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "openfga",
					Containers: []corev1.Container{
						{
							Name:  "openfga",
							Image: image,
							Env: []corev1.EnvVar{
								{Name: "OPENFGA_DATASTORE_ENGINE", Value: "postgres"},
								{Name: "OPENFGA_DATASTORE_URI", Value: "postgres://localhost/openfga"},
								{Name: "OPENFGA_LOG_LEVEL", Value: "info"},
							},
						},
					},
				},
			},
		},
	}
}

func newReconciler(objects ...runtime.Object) *MigrationReconciler {
	scheme := newScheme()
	clientBuilder := fake.NewClientBuilder().WithScheme(scheme)
	for _, obj := range objects {
		clientBuilder = clientBuilder.WithRuntimeObjects(obj)
	}
	return &MigrationReconciler{
		Client:                  clientBuilder.Build(),
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}
}

func TestReconcile_FirstInstall_CreatesJob(t *testing.T) {
	// Given: a Deployment with no migration-status ConfigMap.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: a migration Job should be created and requeue requested.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue, got none")
	}

	// Verify the Job was created.
	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); err != nil {
		t.Fatalf("expected migration job to be created: %v", err)
	}

	if job.Spec.Template.Spec.Containers[0].Image != "openfga/openfga:v1.14.0" {
		t.Errorf("expected job image openfga/openfga:v1.14.0, got %s", job.Spec.Template.Spec.Containers[0].Image)
	}

	if job.Spec.Template.Spec.Containers[0].Args[0] != "migrate" {
		t.Errorf("expected job args [migrate], got %v", job.Spec.Template.Spec.Containers[0].Args)
	}

	// Verify only datastore env vars were passed.
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "OPENFGA_LOG_LEVEL" {
			t.Error("non-datastore env var OPENFGA_LOG_LEVEL should not be passed to migration job")
		}
	}
}

func TestReconcile_VersionMatch_ScalesUp(t *testing.T) {
	// Given: a Deployment at 0 replicas with matching migration-status ConfigMap.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
		},
		Data: map[string]string{
			"version":    "v1.14.0",
			"migratedAt": "2026-04-06T12:00:00Z",
			"jobName":    "openfga-migrate",
		},
	}

	r := newReconciler(dep, cm)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, no requeue.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue when versions match")
	}

	// Verify Deployment was scaled up.
	updated := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	if *updated.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_JobSucceeded_UpdatesConfigMapAndScalesUp(t *testing.T) {
	// Given: a Deployment at 0 replicas, no ConfigMap, and a succeeded migration Job.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "openfga",
					UID:        "test-uid-123",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "migrate", Image: "openfga/openfga:v1.14.0"}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r := newReconciler(dep, job)

	// When: reconciling.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ConfigMap was created.
	cm := &corev1.ConfigMap{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, cm); err != nil {
		t.Fatalf("expected ConfigMap to be created: %v", err)
	}
	if cm.Data["version"] != "v1.14.0" {
		t.Errorf("expected version v1.14.0 in ConfigMap, got %s", cm.Data["version"])
	}

	// Verify Deployment was scaled up.
	updated := &appsv1.Deployment{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	if *updated.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_JobFailed_SetsRetryAnnotationAndRequeues(t *testing.T) {
	// Given: a Deployment at 0 replicas and a failed migration Job.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "openfga",
					UID:        "test-uid-123",
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "migrate", Image: "openfga/openfga:v1.14.0"}},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
		Status: batchv1.JobStatus{
			Failed: 3,
		},
	}

	r := newReconciler(dep, job)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, but requeue after 60s for retry.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("expected 60s requeue, got %v", result.RequeueAfter)
	}

	// Verify Deployment was NOT scaled up — still at 0.
	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 0 {
		t.Errorf("expected 0 replicas after failed migration, got %d", *updated.Spec.Replicas)
	}

	// Verify the failed Job was deleted.
	deletedJob := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, deletedJob); getErr == nil {
		t.Error("expected failed migration job to be deleted")
	}

	// Verify retry-after annotation was set on the Deployment.
	if _, ok := updated.Annotations[AnnotationRetryAfter]; !ok {
		t.Error("expected retry-after annotation to be set on Deployment")
	}
}

func TestReconcile_RetryAfterCooldown_SkipsJobCreation(t *testing.T) {
	// Given: a Deployment with a retry-after annotation in the future.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	dep.Annotations[AnnotationRetryAfter] = time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339)

	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, requeue with remaining cooldown time.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue during cooldown")
	}
	if result.RequeueAfter > 30*time.Second {
		t.Errorf("expected requeue within 30s, got %v", result.RequeueAfter)
	}

	// Verify no Job was created.
	job := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); getErr == nil {
		t.Error("expected no migration job during cooldown")
	}
}

func TestReconcile_MemoryDatastore_SkipsMigration(t *testing.T) {
	// Given: a Deployment using the memory datastore.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "1"
	dep.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "OPENFGA_DATASTORE_ENGINE", Value: "memory"},
	}

	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, no requeue.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue for memory datastore")
	}

	// Verify Deployment was scaled up (no migration needed).
	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %d", *updated.Spec.Replicas)
	}

	// Verify no Job was created.
	job := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); getErr == nil {
		t.Error("expected no migration job for memory datastore")
	}
}

func TestReconcile_DeploymentNotFound_NoError(t *testing.T) {
	r := newReconciler()

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue for missing deployment")
	}
}

func TestReconcile_FindContainerByName(t *testing.T) {
	// Given: a Deployment with a sidecar before the openfga container.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Spec.Template.Spec.Containers = []corev1.Container{
		{
			Name:  "sidecar",
			Image: "envoyproxy/envoy:v1.30",
		},
		{
			Name:  "openfga",
			Image: "openfga/openfga:v1.14.0",
			Env: []corev1.EnvVar{
				{Name: "OPENFGA_DATASTORE_ENGINE", Value: "postgres"},
				{Name: "OPENFGA_DATASTORE_URI", Value: "postgres://localhost/openfga"},
			},
		},
	}

	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: Job should use the openfga container's image, not the sidecar's.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue, got none")
	}

	job := &batchv1.Job{}
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); err != nil {
		t.Fatalf("expected migration job to be created: %v", err)
	}

	if job.Spec.Template.Spec.Containers[0].Image != "openfga/openfga:v1.14.0" {
		t.Errorf("expected job image openfga/openfga:v1.14.0, got %s", job.Spec.Template.Spec.Containers[0].Image)
	}
}

func TestReconcile_MigrationNotEnabled_Skips(t *testing.T) {
	// Given: a Deployment without the migration-enabled annotation.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 3)
	delete(dep.Annotations, AnnotationMigrationEnabled)

	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, no requeue, no Job created, replicas unchanged.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue when migration is not enabled")
	}

	// Verify no Job was created.
	job := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); getErr == nil {
		t.Error("expected no migration job when migration is not enabled")
	}

	// Verify replicas unchanged.
	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 3 {
		t.Errorf("expected 3 replicas unchanged, got %d", *updated.Spec.Replicas)
	}
}

func TestExtractImageTag(t *testing.T) {
	tests := []struct {
		image    string
		expected string
	}{
		{"openfga/openfga:v1.14.0", "v1.14.0"},
		{"openfga/openfga:latest", "latest"},
		{"openfga/openfga", "latest"},
		{"ghcr.io/openfga/openfga:v1.14.0", "v1.14.0"},
		{"registry.example.com:5000/openfga/openfga:v1.14.0", "v1.14.0"},
		{"openfga/openfga@sha256:abcdef1234567890", "sha256:abcdef1234567890"},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			got := extractImageTag(tt.image)
			if got != tt.expected {
				t.Errorf("extractImageTag(%q) = %q, want %q", tt.image, got, tt.expected)
			}
		})
	}
}
