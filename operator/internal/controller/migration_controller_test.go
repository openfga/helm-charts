package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func newLegacyHelmMigrationJob(deployment *appsv1.Deployment, hook string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            migrationJobName(deployment.Name),
			Namespace:       deployment.Namespace,
			UID:             "legacy-job-uid",
			ResourceVersion: "7",
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByHelm,
				LabelInstance:  deployment.Labels[LabelInstance],
				LabelName:      deployment.Labels[LabelName],
				LabelPartOf:    deployment.Labels[LabelPartOf],
				LabelComponent: deployment.Labels[LabelComponent],
			},
			Annotations: map[string]string{
				AnnotationHelmHook: hook,
			},
		},
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

func TestDeleteMigrationJob_UsesObjectPreconditions(t *testing.T) {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "openfga-migrate",
			Namespace:       "default",
			UID:             "job-uid-123",
			ResourceVersion: "7",
		},
	}

	scheme := newScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(job).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
				if deleteOptions.Preconditions == nil {
					t.Fatal("expected delete preconditions")
				}
				if deleteOptions.Preconditions.UID == nil || *deleteOptions.Preconditions.UID != job.UID {
					t.Errorf("expected UID precondition %q", job.UID)
				}
				if deleteOptions.Preconditions.ResourceVersion == nil ||
					*deleteOptions.Preconditions.ResourceVersion != job.ResourceVersion {
					t.Errorf("expected resource-version precondition %q", job.ResourceVersion)
				}
				if deleteOptions.PropagationPolicy == nil ||
					*deleteOptions.PropagationPolicy != metav1.DeletePropagationForeground {
					t.Error("expected foreground deletion")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	if err := deleteMigrationJob(context.Background(), c, job); err != nil {
		t.Fatalf("deleting migration job: %v", err)
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
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

func TestReconcile_VersionMatch_NonZeroReplicasRemainUntouched(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 2)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		Data: map[string]string{"version": "v1.14.0"},
	}
	r := newReconciler(dep, cm)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	updated := &appsv1.Deployment{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(dep), updated); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	if *updated.Spec.Replicas != 2 {
		t.Fatalf("expected live replica count 2 to remain untouched, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_VersionMatch_ConcurrentScaleIsNotOverwritten(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		Data: map[string]string{"version": "v1.14.0"},
	}

	concurrentScaleApplied := false
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep, cm).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				patch client.Patch,
				opts ...client.PatchOption,
			) error {
				if _, ok := obj.(*appsv1.Deployment); ok && !concurrentScaleApplied {
					latest := &appsv1.Deployment{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(obj), latest); err != nil {
						return err
					}
					latest.Spec.Replicas = ptr.To[int32](2)
					if err := c.Update(ctx, latest); err != nil {
						return err
					}
					concurrentScaleApplied = true
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &MigrationReconciler{
		Client:                  c,
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(dep),
	})
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected optimistic-lock conflict, got %v", err)
	}

	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(dep), updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if *updated.Spec.Replicas != 2 {
		t.Fatalf("expected concurrent replica count 2 to remain untouched, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_VersionMatch_ClearedConditionIsIdempotent(t *testing.T) {
	transitionTime := metav1.NewTime(time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC))
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 3)
	dep.Annotations[AnnotationDesiredReplicas] = "3"
	dep.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:               "MigrationFailed",
		Status:             corev1.ConditionFalse,
		LastTransitionTime: transitionTime,
		Reason:             "MigrationSucceeded",
		Message:            "Migration completed successfully.",
	}}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
			},
		},
		Data: map[string]string{"version": "v1.14.0"},
	}

	statusPatchCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep, cm).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				ctx context.Context,
				c client.Client,
				subResourceName string,
				obj client.Object,
				patch client.Patch,
				opts ...client.SubResourcePatchOption,
			) error {
				statusPatchCalls++
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &MigrationReconciler{
		Client:                  c,
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(dep)}

	for range 2 {
		if _, err := r.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("unexpected reconcile error: %v", err)
		}
	}
	if statusPatchCalls != 0 {
		t.Fatalf("expected no repeated status patches, got %d", statusPatchCalls)
	}

	updated := &appsv1.Deployment{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(dep), updated); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	condition := findCondition(updated.Status.Conditions, "MigrationFailed")
	if condition == nil {
		t.Fatal("expected MigrationFailed condition")
	}
	if !condition.LastTransitionTime.Equal(&transitionTime) {
		t.Fatalf(
			"expected transition time %s to remain unchanged, got %s",
			transitionTime,
			condition.LastTransitionTime,
		)
	}
}

func TestReconcile_UnmanagedMatchingConfigMap_ReturnsCollisionError(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
		},
		Data: map[string]string{"version": "v1.14.0"},
	}

	r := newReconciler(dep, cm)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected unmanaged migration status ConfigMap collision to return an error")
	}
	if !strings.Contains(err.Error(), "migration status ConfigMap default/openfga-migration-status exists but is not managed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty result, got %+v", result)
	}

	job := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, job); getErr == nil {
		t.Fatal("expected no migration job to be created")
	}

	unchanged := &corev1.ConfigMap{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, unchanged); getErr != nil {
		t.Fatalf("getting colliding ConfigMap: %v", getErr)
	}
	if unchanged.Data["version"] != "v1.14.0" {
		t.Errorf("expected colliding ConfigMap to remain unchanged, got version %q", unchanged.Data["version"])
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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

func TestReconcile_JobFailureTarget_TreatedAsFailed(t *testing.T) {
	// Given: a Job with only JobFailureTarget=True (no JobFailed yet). The
	// Job controller sets this as soon as it decides the Job will fail,
	// before pods finish terminating and JobFailed is recorded. The operator
	// should treat this as a failure to surface the error in seconds rather
	// than waiting the full BackoffLimit × ActiveDeadlineSeconds.
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	dep.Annotations[AnnotationDesiredReplicas] = "3"

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migrate",
			Namespace: "default",
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
			},
		},
	}

	r := newReconciler(dep, job)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 60*time.Second {
		t.Errorf("expected 60s requeue, got %v", result.RequeueAfter)
	}

	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga", Namespace: "default",
	}, updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}

	deletedJob := &batchv1.Job{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migrate", Namespace: "default",
	}, deletedJob); getErr == nil {
		t.Error("expected migration job to be deleted on JobFailureTarget")
	}
	if _, ok := updated.Annotations[AnnotationRetryAfter]; !ok {
		t.Error("expected retry-after annotation to be set")
	}
	cond := findCondition(updated.Status.Conditions, "MigrationFailed")
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatal("expected MigrationFailed condition True")
	}
}

func TestReconcile_FailedJob_StatusPatchErrorPreservesJob(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	job := buildMigrationJob(
		dep,
		&dep.Spec.Template.Spec.Containers[0],
		"v1.14.0",
		DefaultBackoffLimit,
		DefaultActiveDeadlineSeconds,
		DefaultTTLSecondsAfterFinished,
	)
	job.Status.Conditions = []batchv1.JobCondition{{
		Type:   batchv1.JobFailed,
		Status: corev1.ConditionTrue,
	}}

	deleteCalls := 0
	statusErr := apierrors.NewConflict(
		schema.GroupResource{Group: "apps", Resource: "deployments"},
		dep.Name,
		errors.New("status changed"),
	)
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep, job).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				_ context.Context,
				_ client.Client,
				_ string,
				_ client.Object,
				_ client.Patch,
				_ ...client.SubResourcePatchOption,
			) error {
				return statusErr
			},
			Delete: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				opts ...client.DeleteOption,
			) error {
				deleteCalls++
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	r := &MigrationReconciler{
		Client:                  c,
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(dep),
	})
	if !errors.Is(err, statusErr) {
		t.Fatalf("expected status patch error, got %v", err)
	}
	if deleteCalls != 0 {
		t.Fatalf("expected no Job deletion after status patch failure, got %d calls", deleteCalls)
	}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); getErr != nil {
		t.Fatalf("expected failed Job to remain: %v", getErr)
	}

	updated := &appsv1.Deployment{}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(dep), updated); getErr != nil {
		t.Fatalf("getting deployment: %v", getErr)
	}
	if _, ok := updated.Annotations[AnnotationRetryAfter]; ok {
		t.Fatal("expected retry annotation not to be persisted after status patch failure")
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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
				LabelManagedBy:              LabelManagedByValue,
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

func TestReconcile_TerminatingMigrationJob_WaitsWithoutReplacement(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	job := buildMigrationJob(
		dep,
		&dep.Spec.Template.Spec.Containers[0],
		"v1.14.0",
		DefaultBackoffLimit,
		DefaultActiveDeadlineSeconds,
		DefaultTTLSecondsAfterFinished,
	)
	now := metav1.Now()
	job.DeletionTimestamp = &now
	job.Finalizers = []string{"foregroundDeletion"}

	createCalls := 0
	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep, job).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(
				ctx context.Context,
				c client.WithWatch,
				obj client.Object,
				opts ...client.CreateOption,
			) error {
				if _, ok := obj.(*batchv1.Job); ok {
					createCalls++
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

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKeyFromObject(dep),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("expected one-second deletion poll, got %s", result.RequeueAfter)
	}
	if createCalls != 0 {
		t.Fatalf("expected no replacement while old Job is terminating, got %d creates", createCalls)
	}
}

func TestReconcile_ExistingJob_RequiresManagedLabelAndDeploymentOwner(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	staleJob := buildMigrationJob(
		dep,
		&dep.Spec.Template.Spec.Containers[0],
		"v1.14.0",
		DefaultBackoffLimit,
		DefaultActiveDeadlineSeconds,
		DefaultTTLSecondsAfterFinished,
	)

	tests := []struct {
		name      string
		managed   bool
		owned     bool
		wantError bool
	}{
		{name: "label only", managed: true, wantError: true},
		{name: "owner only", owned: true, wantError: true},
		{name: "both", managed: true, owned: true},
		{name: "neither", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testJob := staleJob.DeepCopy()
			if !tt.managed {
				delete(testJob.Labels, LabelManagedBy)
			}
			if !tt.owned {
				testJob.OwnerReferences = nil
			}

			r := newReconciler(dep.DeepCopy(), testJob)
			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
			})

			existingJob := &batchv1.Job{}
			getErr := r.Get(context.Background(), types.NamespacedName{
				Name: "openfga-migrate", Namespace: "default",
			}, existingJob)
			if tt.wantError {
				if err == nil {
					t.Fatal("expected migration job collision to return an error")
				}
				if !strings.Contains(err.Error(), "must be managed by openfga-operator and owned by Deployment openfga") {
					t.Fatalf("unexpected error: %v", err)
				}
				if getErr != nil {
					t.Fatalf("expected untrusted migration job to remain: %v", getErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.RequeueAfter == 0 {
				t.Fatal("expected requeue after deleting trusted stale job")
			}
			if getErr == nil {
				t.Fatal("expected trusted stale migration job to be deleted")
			}
		})
	}
}

func TestReconcile_CrossChartVersionLegacyHelmMigrationHook_DeletedThenReplaced(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	dep.Labels[LabelInstance] = "authorization"
	dep.Labels[LabelName] = "openfga"
	dep.Labels["helm.sh/chart"] = "openfga-0.4.0"
	legacyJob := newLegacyHelmMigrationJob(dep, "post-install,post-upgrade")
	legacyJob.Labels["helm.sh/chart"] = "openfga-0.3.12"
	r := newReconciler(dep, legacyJob)
	request := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
	}

	result, err := r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error adopting legacy Job: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue after deleting legacy Job")
	}
	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Name: legacyJob.Name, Namespace: legacyJob.Namespace}
	if getErr := r.Get(context.Background(), jobKey, job); getErr == nil {
		t.Fatal("expected matching legacy Helm migration Job to be deleted")
	}

	result, err = r.Reconcile(context.Background(), request)
	if err != nil {
		t.Fatalf("unexpected error replacing legacy Job: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue after creating operator Job")
	}
	if getErr := r.Get(context.Background(), jobKey, job); getErr != nil {
		t.Fatalf("expected operator migration Job to replace legacy Job: %v", getErr)
	}
	if !isOperatorManaged(job) || !isOwnedByDeployment(job, dep) {
		t.Fatal("expected replacement Job to be operator-managed and owned by the Deployment")
	}
}

func TestReconcile_LegacyHelmMigrationHook_MismatchedStableIdentityRejected(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	dep.Labels[LabelInstance] = "authorization"
	dep.Labels[LabelName] = "openfga"

	tests := []struct {
		name  string
		label string
		value string
	}{
		{name: "instance", label: LabelInstance, value: "another-release"},
		{name: "name", label: LabelName, value: "another-app"},
		{name: "part-of", label: LabelPartOf, value: "another-system"},
		{name: "component", label: LabelComponent, value: "another-component"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			legacyJob := newLegacyHelmMigrationJob(dep, "pre-upgrade")
			legacyJob.Labels[tt.label] = tt.value
			r := newReconciler(dep.DeepCopy(), legacyJob)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: client.ObjectKeyFromObject(dep),
			})
			if err == nil {
				t.Fatal("expected mismatched stable release identity to be rejected")
			}
			if getErr := r.Get(
				context.Background(),
				client.ObjectKeyFromObject(legacyJob),
				&batchv1.Job{},
			); getErr != nil {
				t.Fatalf("expected mismatched Helm Job to remain untouched: %v", getErr)
			}
		})
	}
}

func TestReconcile_LegacyHelmMigrationHook_MissingOrInvalidHookRejected(t *testing.T) {
	for _, hook := range []string{"", "test", "backup,cleanup", "pre-upgrade,test"} {
		t.Run(fmt.Sprintf("hook_%q", hook), func(t *testing.T) {
			dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
			dep.Labels[LabelInstance] = "authorization"
			legacyJob := newLegacyHelmMigrationJob(dep, hook)
			r := newReconciler(dep, legacyJob)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
			})
			if err == nil {
				t.Fatal("expected Helm Job without an expected migration hook event to be rejected")
			}
			if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(legacyJob), &batchv1.Job{}); getErr != nil {
				t.Fatalf("expected invalid Helm Job to remain untouched: %v", getErr)
			}
		})
	}
}

func TestReconcile_LegacyHelmMigrationHook_DeleteConflictRequeues(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.15.0", 0)
	dep.Labels[LabelInstance] = "authorization"
	legacyJob := newLegacyHelmMigrationJob(dep, "post-upgrade")
	scheme := newScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithRuntimeObjects(dep, legacyJob).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
				if deleteOptions.Preconditions == nil ||
					deleteOptions.Preconditions.UID == nil ||
					*deleteOptions.Preconditions.UID != legacyJob.UID ||
					deleteOptions.Preconditions.ResourceVersion == nil ||
					*deleteOptions.Preconditions.ResourceVersion != legacyJob.ResourceVersion {
					t.Fatal("expected legacy Job deletion to use UID and resource-version preconditions")
				}
				if deleteOptions.PropagationPolicy == nil ||
					*deleteOptions.PropagationPolicy != metav1.DeletePropagationForeground {
					t.Fatal("expected legacy Job deletion to use foreground propagation")
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: "batch", Resource: "jobs"},
					obj.GetName(),
					errors.New("resource version changed"),
				)
			},
		}).
		Build()
	r := &MigrationReconciler{
		Client:                  c,
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace},
	})
	if err != nil {
		t.Fatalf("expected deletion conflict to requeue without error: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("expected one-second conflict requeue, got %s", result.RequeueAfter)
	}
	if getErr := r.Get(context.Background(), client.ObjectKeyFromObject(legacyJob), &batchv1.Job{}); getErr != nil {
		t.Fatalf("expected conflicted legacy Job to remain: %v", getErr)
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
				LabelManagedBy:              LabelManagedByValue,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "apps/v1",
					Kind:       "Deployment",
					Name:       "openfga",
					UID:        "test-uid-123",
				},
			},
			// No annotation — forces the label-only fallback path.
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

func TestReconcile_JobSucceeded_DoesNotOverwriteUnmanagedConfigMap(t *testing.T) {
	dep := newTestDeployment("openfga", "default", "openfga/openfga:v1.14.0", 0)
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "openfga-migration-status",
			Namespace: "default",
		},
		Data: map[string]string{"version": "user-owned"},
	}

	job := buildMigrationJob(
		dep,
		&dep.Spec.Template.Spec.Containers[0],
		"v1.14.0",
		DefaultBackoffLimit,
		DefaultActiveDeadlineSeconds,
		DefaultTTLSecondsAfterFinished,
	)
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
	}

	r := newReconciler(dep, existingCM, job)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "openfga", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("expected unmanaged migration status ConfigMap collision to return an error")
	}

	cm := &corev1.ConfigMap{}
	if getErr := r.Get(context.Background(), types.NamespacedName{
		Name: "openfga-migration-status", Namespace: "default",
	}, cm); getErr != nil {
		t.Fatalf("getting colliding ConfigMap: %v", getErr)
	}
	if cm.Data["version"] != "user-owned" {
		t.Errorf("expected colliding ConfigMap to remain unchanged, got version %q", cm.Data["version"])
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
				LabelPartOf:         "incorrect",
				LabelComponent:      "incorrect",
				LabelManagedBy:      LabelManagedByValue,
				"example.com/owner": "preserve-me",
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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
	if cm.Labels["example.com/owner"] != "preserve-me" {
		t.Error("expected unrelated ConfigMap label to be preserved")
	}
	for key, expected := range map[string]string{
		LabelPartOf:    LabelPartOfValue,
		LabelComponent: "migration",
		LabelManagedBy: LabelManagedByValue,
	} {
		if cm.Labels[key] != expected {
			t.Errorf("expected required label %s=%q, got %q", key, expected, cm.Labels[key])
		}
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
			Labels: map[string]string{
				LabelManagedBy: LabelManagedByValue,
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
