package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	clientBuilder := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&appsv1.Deployment{})
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

func findCondition(conditions []appsv1.DeploymentCondition, condType string) *appsv1.DeploymentCondition {
	for i := range conditions {
		if string(conditions[i].Type) == condType {
			return &conditions[i]
		}
	}
	return nil
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

	// Verify all env vars from the main container were passed.
	jobEnvNames := make(map[string]bool)
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		jobEnvNames[env.Name] = true
	}
	for _, expected := range []string{"OPENFGA_DATASTORE_ENGINE", "OPENFGA_DATASTORE_URI", "OPENFGA_LOG_LEVEL"} {
		if !jobEnvNames[expected] {
			t.Errorf("expected env var %s to be passed to migration job", expected)
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
	// Given: a Deployment at 0 replicas, no ConfigMap, a succeeded migration Job,
	// and a pre-existing MigrationFailed condition from a prior attempt.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	dep.Status.Conditions = []appsv1.DeploymentCondition{
		{
			Type:    "MigrationFailed",
			Status:  corev1.ConditionTrue,
			Reason:  "MigrationJobFailed",
			Message: "Database migration failed for version v1.13.0.",
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Annotations: map[string]string{
				"openfga.dev/desired-version": "v1.14.0",
			},
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
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
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

	// Verify MigrationFailed condition was cleared.
	cond := findCondition(updated.Status.Conditions, "MigrationFailed")
	if cond == nil {
		t.Fatal("expected MigrationFailed condition to exist")
	}
	if cond.Status != corev1.ConditionFalse {
		t.Errorf("expected MigrationFailed status False after success, got %s", cond.Status)
	}
	if cond.Reason != "MigrationSucceeded" {
		t.Errorf("expected reason MigrationSucceeded, got %s", cond.Reason)
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
			Annotations: map[string]string{
				"openfga.dev/desired-version": "v1.14.0",
			},
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
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
				},
			},
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

	// Verify Deployment replicas unchanged (still at 0 from fresh install).
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

	// Verify MigrationFailed condition was set.
	cond := findCondition(updated.Status.Conditions, "MigrationFailed")
	if cond == nil {
		t.Fatal("expected MigrationFailed condition to be set")
	}
	if cond.Status != corev1.ConditionTrue {
		t.Errorf("expected MigrationFailed status True, got %s", cond.Status)
	}
	if cond.Reason != "MigrationJobFailed" {
		t.Errorf("expected reason MigrationJobFailed, got %s", cond.Reason)
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

func TestReconcile_UnknownVersionJob_DeletedNotTrusted(t *testing.T) {
	// Given: a Deployment desiring v1.14.0 and a JobComplete migration Job that
	// carries no version annotation or label (e.g. left over from an older
	// operator or created by a third-party tool). Trusting its outcome would
	// write the wrong version into the migration-status ConfigMap.
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
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}

	r := newReconciler(dep, job)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: the Job is deleted and a requeue is scheduled; the ConfigMap is
	// NOT created from the unknown-version Job's outcome.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after deleting unknown-version job")
	}

	deletedJob := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, deletedJob); getErr == nil {
		t.Error("expected unknown-version job to be deleted")
	}

	cm := &corev1.ConfigMap{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, cm); getErr == nil {
		t.Errorf("expected no migration-status ConfigMap; got version=%q", cm.Data["version"])
	}
}

func TestReconcile_RetryAfterPersistsOnJobCreateFailure(t *testing.T) {
	// Given: a Deployment with an elapsed retry-after annotation, and a client
	// that fails Job creation with a non-AlreadyExists error.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	dep.Annotations[AnnotationRetryAfter] = time.Now().Add(-1 * time.Second).UTC().Format(time.RFC3339)

	scheme := newScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*batchv1.Job); ok {
					return fmt.Errorf("simulated transient API error")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &MigrationReconciler{
		Client:                  c,
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}

	// When: reconciling.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: an error is returned and the retry-after annotation is preserved
	// so the next reconcile honors the cooldown.
	if err == nil {
		t.Fatal("expected error from failed job creation")
	}

	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if _, ok := updated.Annotations[AnnotationRetryAfter]; !ok {
		t.Error("expected retry-after annotation to persist after Job creation failure")
	}
}

func TestReconcile_RetryAfterClearedAfterJobCreated(t *testing.T) {
	// Given: a Deployment with an elapsed retry-after annotation.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	dep.Annotations[AnnotationRetryAfter] = time.Now().Add(-1 * time.Second).UTC().Format(time.RFC3339)

	r := newReconciler(dep)

	// When: reconciling.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Then: the Job exists and the retry-after annotation has been cleared.
	job := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); getErr != nil {
		t.Fatalf("expected migration job to be created: %v", getErr)
	}

	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if _, ok := updated.Annotations[AnnotationRetryAfter]; ok {
		t.Error("expected retry-after annotation to be cleared after Job created")
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

func TestReconcile_StaleJob_DeletedAndRequeued(t *testing.T) {
	// Given: a Deployment at v1.15.0 with an existing migration Job for v1.14.0.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	staleJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/version": "v1.14.0",
			},
			Annotations: map[string]string{
				"openfga.dev/desired-version": "v1.14.0",
			},
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
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	r := newReconciler(dep, staleJob)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, requeue to recreate with correct version.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after deleting stale job")
	}

	// Verify the stale Job was deleted.
	deletedJob := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, deletedJob); getErr == nil {
		t.Error("expected stale migration job to be deleted")
	}

	// Verify ConfigMap was NOT updated (migration didn't actually run for v1.15.0).
	cm := &corev1.ConfigMap{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, cm); getErr == nil {
		if cm.Data["version"] == "v1.15.0" {
			t.Error("ConfigMap should not be updated to v1.15.0 from a stale v1.14.0 job")
		}
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

func TestReconcile_StaleJob_LabelOnlyFallback_DeletedAndRequeued(t *testing.T) {
	// Given: a Deployment at v1.15.0 with an existing Job that only has a label (no annotation).
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	staleJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/version": "v1.14.0",
			},
			// No annotation — forces the label-only fallback path.
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
	}

	r := newReconciler(dep, staleJob)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: stale Job should be deleted and requeue requested.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after deleting stale job")
	}

	deletedJob := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, deletedJob); getErr == nil {
		t.Error("expected stale migration job to be deleted")
	}
}

func TestReconcile_JobSucceeded_UpdatesExistingConfigMap(t *testing.T) {
	// Given: a Deployment with a pre-existing ConfigMap from v1.13.0 and a succeeded Job for v1.14.0.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
			Labels: map[string]string{
				LabelPartOf:    LabelPartOfValue,
				LabelComponent: "migration",
				"app.kubernetes.io/managed-by": "openfga-operator",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "openfga",
					UID:        "test-uid-123",
				},
			},
		},
		Data: map[string]string{
			"version":    "v1.13.0",
			"migratedAt": "2026-04-01T12:00:00Z",
			"jobName":    "openfga-migrate",
		},
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Annotations: map[string]string{
				"openfga.dev/desired-version": "v1.14.0",
			},
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
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	r := newReconciler(dep, existingCM, job)

	// When: reconciling.
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ConfigMap was updated to v1.14.0.
	cm := &corev1.ConfigMap{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, cm); getErr != nil {
		t.Fatalf("expected ConfigMap to exist: %v", getErr)
	}
	if cm.Data["version"] != "v1.14.0" {
		t.Errorf("expected version v1.14.0 in ConfigMap, got %s", cm.Data["version"])
	}
}

func TestReconcile_MigrationNeeded_DoesNotScaleToZero(t *testing.T) {
	// Given: a Deployment with replicas > 0 and no migration-status ConfigMap.
	// The operator should create the migration Job WITHOUT scaling to zero,
	// relying on OpenFGA's built-in schema version check to gate readiness.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 3)
	dep.Annotations = map[string]string{
		AnnotationMigrationEnabled: "true",
		AnnotationDesiredReplicas:  "3",
	}

	r := newReconciler(dep)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, Job created.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after creating job")
	}

	// Verify Deployment replicas were NOT changed — pods keep running during migration.
	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 3 {
		t.Errorf("expected replicas to remain at 3, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_JobInProgress_Requeues(t *testing.T) {
	// Given: a Deployment with a running Job (no conditions set yet).
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Annotations: map[string]string{
				"openfga.dev/desired-version": "v1.14.0",
			},
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
			Active: 1,
		},
	}

	r := newReconciler(dep, job)

	// When: reconciling.
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})

	// Then: no error, requeue after 10s to poll progress.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected 10s requeue for in-progress job, got %v", result.RequeueAfter)
	}

	// Verify Deployment still at 0 replicas.
	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 0 {
		t.Errorf("expected 0 replicas while job in progress, got %d", *updated.Spec.Replicas)
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
