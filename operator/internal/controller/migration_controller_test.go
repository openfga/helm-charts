package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

var (
	deploymentKey = types.NamespacedName{Name: "openfga", Namespace: "default"}
	jobKey        = types.NamespacedName{Name: "openfga-migrate", Namespace: "default"}
	statusKey     = types.NamespacedName{Name: "openfga-migration-status", Namespace: "default"}
)

func newTestDeployment(image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentKey.Name,
			Namespace: deploymentKey.Namespace,
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
			Replicas: ptr.To(int32(3)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "openfga"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "openfga"}},
				Spec: corev1.PodSpec{
					ServiceAccountName: "openfga",
					Containers: []corev1.Container{{
						Name:  "openfga",
						Image: image,
						Env: []corev1.EnvVar{
							{Name: "OPENFGA_DATASTORE_ENGINE", Value: "postgres"},
							{Name: "OPENFGA_DATASTORE_URI", Value: "postgres://localhost/openfga"},
							{Name: "OPENFGA_LOG_LEVEL", Value: "info"},
						},
					}},
				},
			},
		},
	}
}

// newTestJob returns the migration Job the operator builds for dep.
func newTestJob(dep *appsv1.Deployment, conditions ...batchv1.JobCondition) *batchv1.Job {
	container := &dep.Spec.Template.Spec.Containers[0]
	job, err := (&MigrationReconciler{}).buildMigrationJob(dep, container, extractImageTag(container.Image))
	if err != nil {
		panic(err)
	}
	job.Status.Conditions = conditions
	return job
}

func jobCondition(t batchv1.JobConditionType, at time.Time) batchv1.JobCondition {
	return batchv1.JobCondition{Type: t, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(at)}
}

func newStatus(dep *appsv1.Deployment) *corev1.ConfigMap {
	job := newTestJob(dep)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statusKey.Name,
			Namespace: statusKey.Namespace,
			Labels:    map[string]string{LabelManagedBy: LabelManagedByValue},
			OwnerReferences: []metav1.OwnerReference{
				ownerReference(dep),
			},
		},
		Data: map[string]string{
			"version":         job.Annotations[AnnotationDesiredVersion],
			"podTemplateHash": job.Annotations[AnnotationPodTemplateHash],
		},
	}
}

func newReconciler(t *testing.T, funcs *interceptor.Funcs, objects ...client.Object) *MigrationReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	b := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&appsv1.Deployment{}).
		WithObjects(objects...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return &MigrationReconciler{
		Client:                  b.Build(),
		BackoffLimit:            DefaultBackoffLimit,
		ActiveDeadlineSeconds:   DefaultActiveDeadlineSeconds,
		TTLSecondsAfterFinished: DefaultTTLSecondsAfterFinished,
	}
}

var failStatusPatch = &interceptor.Funcs{
	SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		if subResource == "status" {
			return fmt.Errorf("simulated status patch error")
		}
		return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
	},
}

func reconcileOnce(t *testing.T, r *MigrationReconciler) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey})
	if err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	return result
}

func getDeployment(t *testing.T, r *MigrationReconciler) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := r.Get(context.Background(), deploymentKey, d); err != nil {
		t.Fatalf("getting deployment: %v", err)
	}
	return d
}

func getJob(r *MigrationReconciler) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	return job, r.Get(context.Background(), jobKey, job)
}

func getStatus(r *MigrationReconciler) (*corev1.ConfigMap, error) {
	cm := &corev1.ConfigMap{}
	return cm, r.Get(context.Background(), statusKey, cm)
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
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Annotations[AnnotationMigrationServiceAccount] = "openfga-migration"
	dep.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
	r := newReconciler(t, nil, dep)

	if result := reconcileOnce(t, r); result.RequeueAfter == 0 {
		t.Error("expected a requeue to poll the new Job")
	}

	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected migration job to be created: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "openfga/openfga:v1.14.0" || len(c.Args) != 1 || c.Args[0] != "migrate" {
		t.Errorf("unexpected migrate container: image=%s args=%v", c.Image, c.Args)
	}
	if c.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("expected the OpenFGA container's pull policy, got %q", c.ImagePullPolicy)
	}
	if len(c.Env) != 3 {
		t.Errorf("expected all OpenFGA env vars to be passed through, got %v", c.Env)
	}
	if sa := job.Spec.Template.Spec.ServiceAccountName; sa != "openfga-migration" {
		t.Errorf("expected migration service account, got %q", sa)
	}
	if got := job.Annotations[AnnotationDesiredVersion]; got != "v1.14.0" {
		t.Errorf("expected desired-version annotation v1.14.0, got %q", got)
	}
	if job.Spec.ActiveDeadlineSeconds != nil {
		t.Errorf("expected no deadline by default, got %d", *job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("expected TTL to remain unset until the Job succeeds, got %d", *job.Spec.TTLSecondsAfterFinished)
	}
	if len(job.OwnerReferences) != 1 || !ptr.Deref(job.OwnerReferences[0].Controller, false) || job.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Errorf("expected a single controller owner reference without blockOwnerDeletion, got %+v", job.OwnerReferences)
	}
	if replicas := *getDeployment(t, r).Spec.Replicas; replicas != 3 {
		t.Errorf("replicas must not be changed, got %d", replicas)
	}
}

func TestReconcile_FirstInstall_DeadlineAndDefaultServiceAccount(t *testing.T) {
	r := newReconciler(t, nil, newTestDeployment("openfga/openfga:v1.14.0"))
	r.ActiveDeadlineSeconds = 600
	reconcileOnce(t, r)

	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected migration job to be created: %v", err)
	}
	if got := ptr.Deref(job.Spec.ActiveDeadlineSeconds, 0); got != 600 {
		t.Errorf("expected activeDeadlineSeconds 600, got %d", got)
	}
	if sa := job.Spec.Template.Spec.ServiceAccountName; sa != "openfga" {
		t.Errorf("expected the Deployment's service account, got %q", sa)
	}
}

func TestReconcile_JobAlreadyExistsOnCreate_Requeues(t *testing.T) {
	r := newReconciler(t, &interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			return apierrors.NewAlreadyExists(batchv1.Resource("jobs"), obj.GetName())
		},
	}, newTestDeployment("openfga/openfga:v1.14.0"))

	if result := reconcileOnce(t, r); result.RequeueAfter == 0 {
		t.Error("expected a requeue when the Job already exists")
	}
}

func TestReconcile_VersionMatch_ClearsFailedCondition(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: "MigrationFailed", Status: corev1.ConditionTrue}}
	r := newReconciler(t, nil, dep, newStatus(dep))

	if result := reconcileOnce(t, r); result.RequeueAfter != 0 {
		t.Errorf("expected no requeue when versions match, got %v", result.RequeueAfter)
	}
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected no migration job, got err=%v", err)
	}
	updated := getDeployment(t, r)
	if cond := findCondition(updated.Status.Conditions, "MigrationFailed"); cond == nil || cond.Status != corev1.ConditionFalse {
		t.Errorf("expected MigrationFailed=False, got %+v", cond)
	}
	if *updated.Spec.Replicas != 3 {
		t.Errorf("replicas must not be changed, got %d", *updated.Spec.Replicas)
	}
}

func TestReconcile_VersionMatch_StatusPatchError(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: "MigrationFailed", Status: corev1.ConditionTrue}}
	r := newReconciler(t, failStatusPatch, dep, newStatus(dep))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
		t.Fatal("expected the status patch error to be returned")
	}
}

func TestReconcile_JobSucceeded_CreatesStatus(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: "MigrationFailed", Status: corev1.ConditionTrue, Reason: "MigrationJobFailed"}}
	job := newTestJob(dep, jobCondition(batchv1.JobComplete, time.Now()))
	r := newReconciler(t, nil, dep, job)

	if result := reconcileOnce(t, r); result.RequeueAfter != 0 {
		t.Errorf("expected no requeue after success, got %v", result.RequeueAfter)
	}
	cm, err := getStatus(r)
	if err != nil {
		t.Fatalf("expected migration status ConfigMap: %v", err)
	}
	if cm.Data["version"] != "v1.14.0" || cm.Data["podTemplateHash"] != job.Annotations[AnnotationPodTemplateHash] || cm.Data["jobName"] != jobKey.Name {
		t.Errorf("unexpected status data: %v", cm.Data)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "test-uid-123" {
		t.Errorf("expected the Deployment to own the status ConfigMap, got %+v", cm.OwnerReferences)
	}
	completedJob, err := getJob(r)
	if err != nil {
		t.Fatalf("expected the completed Job: %v", err)
	}
	if got := ptr.Deref(completedJob.Spec.TTLSecondsAfterFinished, -1); got != DefaultTTLSecondsAfterFinished {
		t.Errorf("expected completed Job TTL %d, got %d", DefaultTTLSecondsAfterFinished, got)
	}
	cond := findCondition(getDeployment(t, r).Status.Conditions, "MigrationFailed")
	if cond == nil || cond.Status != corev1.ConditionFalse || cond.Reason != "MigrationSucceeded" {
		t.Errorf("expected MigrationFailed=False/MigrationSucceeded, got %+v", cond)
	}
}

func TestReconcile_JobSucceeded_AppliesZeroTTL(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	r := newReconciler(t, nil, dep, newTestJob(dep, jobCondition(batchv1.JobComplete, time.Now())))
	r.TTLSecondsAfterFinished = 0

	reconcileOnce(t, r)

	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected the fake client to retain the completed Job: %v", err)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 0 {
		t.Errorf("expected completed Job TTL 0, got %v", job.Spec.TTLSecondsAfterFinished)
	}
}

func TestReconcile_JobSucceeded_UpdatesStatus(t *testing.T) {
	oldDep := newTestDeployment("openfga/openfga:v1.13.0")
	oldDep.UID = "old-uid"
	status := newStatus(oldDep)
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	job := newTestJob(dep, jobCondition(batchv1.JobComplete, time.Now()))
	r := newReconciler(t, nil, dep, status, job)

	reconcileOnce(t, r)

	cm, err := getStatus(r)
	if err != nil {
		t.Fatalf("expected migration status ConfigMap: %v", err)
	}
	if cm.Data["version"] != "v1.14.0" {
		t.Errorf("expected version v1.14.0, got %q", cm.Data["version"])
	}
	if cm.Data["podTemplateHash"] != job.Annotations[AnnotationPodTemplateHash] {
		t.Errorf("expected the completed Job's pod template hash, got %q", cm.Data["podTemplateHash"])
	}
	if cm.OwnerReferences[0].UID != "test-uid-123" {
		t.Errorf("expected owner reference to be reset to the current Deployment, got %+v", cm.OwnerReferences)
	}
}

func TestReconcile_JobInProgress_Requeues(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	r := newReconciler(t, nil, dep, newTestJob(dep))

	if result := reconcileOnce(t, r); result.RequeueAfter != 10*time.Second {
		t.Errorf("expected 10s requeue for an in-progress job, got %v", result.RequeueAfter)
	}
	if _, err := getStatus(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected no status ConfigMap while the job runs, got err=%v", err)
	}
}

func TestReconcile_JobFailed_KeepsJobUntilRetryDelay(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	r := newReconciler(t, nil, dep, newTestJob(dep, jobCondition(batchv1.JobFailed, time.Now().Add(-10*time.Second))))

	result := reconcileOnce(t, r)
	if result.RequeueAfter <= 0 || result.RequeueAfter > retryDelay-10*time.Second {
		t.Errorf("expected a requeue for the rest of the retry delay, got %v", result.RequeueAfter)
	}
	if _, err := getJob(r); err != nil {
		t.Errorf("expected the failed job to be kept during the retry delay: %v", err)
	}
	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected the failed Job: %v", err)
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("failed Jobs must not have a completion TTL, got %d", *job.Spec.TTLSecondsAfterFinished)
	}
	cond := findCondition(getDeployment(t, r).Status.Conditions, "MigrationFailed")
	if cond == nil || cond.Status != corev1.ConditionTrue || cond.Reason != "MigrationJobFailed" {
		t.Errorf("expected MigrationFailed=True, got %+v", cond)
	}
}

func TestReconcile_JobFailed_RetriesAfterDelay(t *testing.T) {
	for _, condType := range []batchv1.JobConditionType{batchv1.JobFailed, batchv1.JobFailureTarget} {
		t.Run(string(condType), func(t *testing.T) {
			dep := newTestDeployment("openfga/openfga:v1.14.0")
			r := newReconciler(t, nil, dep, newTestJob(dep, jobCondition(condType, time.Now().Add(-2*retryDelay))))

			reconcileOnce(t, r)
			if _, err := getJob(r); !apierrors.IsNotFound(err) {
				t.Fatalf("expected the failed job to be deleted, got err=%v", err)
			}
			if cond := findCondition(getDeployment(t, r).Status.Conditions, "MigrationFailed"); cond == nil || cond.Status != corev1.ConditionTrue {
				t.Errorf("expected MigrationFailed=True, got %+v", cond)
			}

			reconcileOnce(t, r)
			job, err := getJob(r)
			if err != nil {
				t.Fatalf("expected a new migration job: %v", err)
			}
			if len(job.Status.Conditions) != 0 {
				t.Errorf("expected a fresh job, got conditions %+v", job.Status.Conditions)
			}
		})
	}
}

func TestReconcile_JobFailed_StatusPatchErrorKeepsJob(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	r := newReconciler(t, failStatusPatch, dep, newTestJob(dep, jobCondition(batchv1.JobFailed, time.Now().Add(-2*retryDelay))))

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
		t.Fatal("expected the status patch error to be returned")
	}
	if _, err := getJob(r); err != nil {
		t.Errorf("the failed job must be kept until the failure is recorded: %v", err)
	}
}

func TestReconcile_JobForOtherVersion_Replaced(t *testing.T) {
	r := newReconciler(t, nil, newTestDeployment("openfga/openfga:v1.15.0"),
		newTestJob(newTestDeployment("openfga/openfga:v1.14.0"), jobCondition(batchv1.JobComplete, time.Now())))

	if result := reconcileOnce(t, r); result.RequeueAfter == 0 {
		t.Error("expected a requeue after deleting the stale job")
	}
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected the stale job to be deleted, got err=%v", err)
	}
	if _, err := getStatus(r); !apierrors.IsNotFound(err) {
		t.Errorf("a job for another version must not be recorded as migrated, got err=%v", err)
	}
}

func TestReconcile_PendingJobWithOutdatedTemplate_Replaced(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	job := newTestJob(dep)
	job.Status.Active = 1
	job.Status.Ready = ptr.To(int32(0))
	dep.Spec.Template.Spec.Containers[0].Env[1].Value = "postgres://db.example.com/openfga"
	r := newReconciler(t, nil, dep, job)

	reconcileOnce(t, r)
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the job built from the old pod template to be deleted, got err=%v", err)
	}

	reconcileOnce(t, r)
	rebuilt, err := getJob(r)
	if err != nil {
		t.Fatalf("expected a new migration job: %v", err)
	}
	if uri := rebuilt.Spec.Template.Spec.Containers[0].Env[1].Value; uri != "postgres://db.example.com/openfga" {
		t.Errorf("expected the new job to use the updated env, got %q", uri)
	}
}

func TestReconcile_StatusWithOutdatedMigrationInputs_Reruns(t *testing.T) {
	tests := []struct {
		name   string
		change func(*appsv1.Deployment)
	}{
		{
			name: "datastore URI",
			change: func(dep *appsv1.Deployment) {
				dep.Spec.Template.Spec.Containers[0].Env[1].Value = "postgres://other.example.com/openfga"
			},
		},
		{
			name: "migration nonce",
			change: func(dep *appsv1.Deployment) {
				dep.Annotations[AnnotationMigrationNonce] = "secret-rotation-2"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dep := newTestDeployment("openfga/openfga:v1.14.0")
			status := newStatus(dep)
			tt.change(dep)
			r := newReconciler(t, nil, dep, status)

			reconcileOnce(t, r)
			job, err := getJob(r)
			if err != nil {
				t.Fatalf("expected changed migration inputs to create a Job: %v", err)
			}
			if job.Annotations[AnnotationPodTemplateHash] == status.Data["podTemplateHash"] {
				t.Error("expected changed migration inputs to produce a new identity")
			}
		})
	}
}

func TestReconcile_VersionOnlyStatus_Reruns(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	status := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            statusKey.Name,
			Namespace:       statusKey.Namespace,
			Labels:          map[string]string{LabelManagedBy: LabelManagedByValue},
			OwnerReferences: []metav1.OwnerReference{ownerReference(dep)},
		},
		Data: map[string]string{"version": "v1.14.0"},
	}
	r := newReconciler(t, nil, dep, status)

	reconcileOnce(t, r)
	if _, err := getJob(r); err != nil {
		t.Fatalf("expected legacy version-only status to be migrated to the new identity: %v", err)
	}
}

func TestReconcile_StatusOwnedByPreviousDeployment_Reruns(t *testing.T) {
	oldDep := newTestDeployment("openfga/openfga:v1.14.0")
	oldDep.UID = "old-deployment-uid"
	status := newStatus(oldDep)
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	r := newReconciler(t, nil, dep, status)

	reconcileOnce(t, r)
	if _, err := getJob(r); err != nil {
		t.Fatalf("expected a new Deployment to rerun the migration: %v", err)
	}
}

func TestReconcile_UnownedStatusCollision_ReturnsError(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	status := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: statusKey.Name, Namespace: statusKey.Namespace},
		Data:       map[string]string{"version": "v1.14.0"},
	}
	r := newReconciler(t, nil, dep, status)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
		t.Fatal("expected an unowned status ConfigMap collision to return an error")
	}
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("the collision must prevent migration Job creation, got err=%v", err)
	}
}

func TestReconcile_StartedJobWithOutdatedTemplate_Kept(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status batchv1.JobStatus
	}{
		{"pod running", batchv1.JobStatus{Active: 1, Ready: ptr.To(int32(1))}},
		{"pod finished before the job is marked complete", batchv1.JobStatus{Succeeded: 1, Ready: ptr.To(int32(0))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dep := newTestDeployment("openfga/openfga:v1.14.0")
			job := newTestJob(dep)
			job.Status = tt.status
			dep.Spec.Template.Spec.Containers[0].Env[1].Value = "postgres://db.example.com/openfga"
			r := newReconciler(t, nil, dep, job)

			if result := reconcileOnce(t, r); result.RequeueAfter != 10*time.Second {
				t.Errorf("expected the job to be polled, got %v", result.RequeueAfter)
			}
			if _, err := getJob(r); err != nil {
				t.Errorf("a started migration must not be replaced: %v", err)
			}
		})
	}
}

func TestReconcile_RunningJobForPreviousVersion_KeptUntilItEnds(t *testing.T) {
	// The image changes from v1.14.0 to v1.15.0 while the v1.14.0 migration is
	// running. Interrupting it could leave the schema half-migrated, so the Job
	// must be left alone and only replaced once it finishes.
	oldDep := newTestDeployment("openfga/openfga:v1.14.0")
	job := newTestJob(oldDep)
	job.Status.Active = 1
	job.Status.Ready = ptr.To(int32(1))
	dep := newTestDeployment("openfga/openfga:v1.15.0")
	r := newReconciler(t, nil, dep, job)

	if result := reconcileOnce(t, r); result.RequeueAfter != 10*time.Second {
		t.Errorf("expected the running job to be polled, got %v", result.RequeueAfter)
	}
	kept, err := getJob(r)
	if err != nil {
		t.Fatalf("a running migration must not be deleted on a version change: %v", err)
	}
	if _, err := getStatus(r); !apierrors.IsNotFound(err) {
		t.Errorf("no version may be recorded while the old job runs, got err=%v", err)
	}

	kept.Status = batchv1.JobStatus{Succeeded: 1, Ready: ptr.To(int32(0)), Conditions: []batchv1.JobCondition{jobCondition(batchv1.JobComplete, time.Now())}}
	if err := r.Status().Update(context.Background(), kept); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, r)
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Fatalf("expected the finished v1.14.0 job to be replaced, got err=%v", err)
	}
	if _, err := getStatus(r); !apierrors.IsNotFound(err) {
		t.Errorf("a v1.14.0 job must not be recorded as a v1.15.0 migration, got err=%v", err)
	}

	reconcileOnce(t, r)
	replacement, err := getJob(r)
	if err != nil {
		t.Fatalf("expected a migration job for the new version: %v", err)
	}
	if got := replacement.Annotations[AnnotationDesiredVersion]; got != "v1.15.0" {
		t.Errorf("expected the new job to target v1.15.0, got %q", got)
	}
}

func TestReconcile_UnownedJobCollision_ReturnsError(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	external := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: jobKey.Namespace}}
	r := newReconciler(t, nil, dep, external)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
		t.Fatal("expected an unowned Job collision to return an error")
	}
	if _, err := getJob(r); err != nil {
		t.Errorf("the unowned Job must not be deleted: %v", err)
	}
}

func TestReconcile_JobOwnedByPreviousDeployment_ReplacedWithPreconditions(t *testing.T) {
	oldDep := newTestDeployment("openfga/openfga:v1.14.0")
	oldDep.UID = "old-deployment-uid"
	job := newTestJob(oldDep)
	job.UID = "old-job-uid"
	job.ResourceVersion = "7"
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	var checkedPreconditions bool
	r := newReconciler(t, &interceptor.Funcs{
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			applied := (&client.DeleteOptions{}).ApplyOptions(opts)
			if applied.Preconditions == nil ||
				applied.Preconditions.UID == nil ||
				*applied.Preconditions.UID != job.UID ||
				applied.Preconditions.ResourceVersion == nil ||
				*applied.Preconditions.ResourceVersion != job.ResourceVersion {
				return fmt.Errorf("missing delete preconditions: %+v", applied.Preconditions)
			}
			if applied.PropagationPolicy == nil || *applied.PropagationPolicy != metav1.DeletePropagationForeground {
				return fmt.Errorf("expected foreground deletion, got %v", applied.PropagationPolicy)
			}
			checkedPreconditions = true
			return c.Delete(ctx, obj, opts...)
		},
	}, dep, job)

	reconcileOnce(t, r)
	if !checkedPreconditions {
		t.Fatal("expected Job deletion to include UID and resourceVersion preconditions")
	}
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected the stale operator Job to be deleted, got err=%v", err)
	}
}

// The legacy chart's Helm hook Job has the same name and carries the chart's
// app.kubernetes.io/version label, which is the chart appVersion rather than
// the image it ran. It must never be taken as proof of a migration.
func TestReconcile_LegacyHookJob_NotTrusted(t *testing.T) {
	legacy := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobKey.Name,
			Namespace: jobKey.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/version":    "v1.14.0",
				"app.kubernetes.io/managed-by": "Helm",
			},
			Annotations: map[string]string{"helm.sh/hook": "post-install, post-upgrade, post-rollback, post-delete"},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{jobCondition(batchv1.JobComplete, time.Now())}},
	}
	r := newReconciler(t, nil, newTestDeployment("openfga/openfga:v1.14.0"), legacy)

	reconcileOnce(t, r)
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected the legacy hook job to be deleted, got err=%v", err)
	}
	if _, err := getStatus(r); !apierrors.IsNotFound(err) {
		t.Errorf("the legacy hook job must not be recorded as a migration, got err=%v", err)
	}

	reconcileOnce(t, r)
	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected a new migration job: %v", err)
	}
	if job.Annotations[AnnotationDesiredVersion] != "v1.14.0" {
		t.Errorf("expected the new job to target v1.14.0, got %v", job.Annotations)
	}
}

func TestReconcile_MigrationNotEnabled_Skips(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	delete(dep.Annotations, AnnotationMigrationEnabled)
	r := newReconciler(t, nil, dep)

	if result := reconcileOnce(t, r); result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
	if _, err := getJob(r); !apierrors.IsNotFound(err) {
		t.Errorf("expected no migration job, got err=%v", err)
	}
}

func TestReconcile_DeploymentNotFound_NoError(t *testing.T) {
	r := newReconciler(t, nil)
	if result := reconcileOnce(t, r); result.RequeueAfter != 0 {
		t.Errorf("expected no requeue, got %v", result.RequeueAfter)
	}
}

func TestReconcile_ContainerFromAnnotation(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Annotations[AnnotationContainerName] = "server"
	dep.Spec.Template.Spec.Containers = []corev1.Container{
		{Name: "sidecar", Image: "envoyproxy/envoy:v1.30.0"},
		{Name: "server", Image: "openfga/openfga:v1.14.0"},
	}
	r := newReconciler(t, nil, dep)
	reconcileOnce(t, r)

	job, err := getJob(r)
	if err != nil {
		t.Fatalf("expected migration job to be created: %v", err)
	}
	if image := job.Spec.Template.Spec.Containers[0].Image; image != "openfga/openfga:v1.14.0" {
		t.Errorf("expected the annotated container's image, got %s", image)
	}
}

func TestReconcile_ContainerNotFound_ReturnsError(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Spec.Template.Spec.Containers[0].Name = "server"
	r := newReconciler(t, nil, dep)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
		t.Fatal("expected an error when the OpenFGA container is missing")
	}
}

func TestBuildMigrationJob_UsesMigrationPodConfiguration(t *testing.T) {
	dep := newTestDeployment("openfga/openfga:v1.14.0")
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: "shared",
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}}
	dep.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "shared", MountPath: "/shared"}}
	dep.Annotations[AnnotationMigrationInitContainers] = `[{"name":"prepare-proxy","image":"busybox:1.36"}]`
	dep.Annotations[AnnotationMigrationSidecars] = `[{"name":"database-proxy","image":"proxy:v2"}]`
	dep.Annotations[AnnotationMigrationVolumes] = `[{"name":"credentials","secret":{"secretName":"database-proxy"}}]`
	dep.Annotations[AnnotationMigrationVolumeMounts] = `[{"name":"credentials","mountPath":"/credentials","readOnly":true}]`
	dep.Annotations[AnnotationMigrationResources] = `{"requests":{"cpu":"100m"}}`
	dep.Annotations[AnnotationMigrationTimeout] = "2m"
	dep.Annotations[AnnotationMigrationNonce] = "secret-rotation-2"
	dep.Annotations[AnnotationMigrationAnnotations] = `{"admission.example.com/inject":"enabled","openfga.dev/migration-nonce":"ignored"}`
	dep.Annotations[AnnotationMigrationLabels] = `{"app.kubernetes.io/component":"overridden","network-policy.example.com/database":"allowed"}`

	job := newTestJob(dep)
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "prepare-proxy" {
		t.Errorf("unexpected init containers: %+v", spec.InitContainers)
	}
	if len(spec.Containers) != 2 || spec.Containers[1].Name != "database-proxy" {
		t.Errorf("unexpected containers: %+v", spec.Containers)
	}
	if len(spec.Volumes) != 2 || spec.Volumes[1].Name != "credentials" {
		t.Errorf("unexpected volumes: %+v", spec.Volumes)
	}
	migrate := spec.Containers[0]
	if len(migrate.VolumeMounts) != 2 || migrate.VolumeMounts[1].MountPath != "/credentials" {
		t.Errorf("unexpected migration volume mounts: %+v", migrate.VolumeMounts)
	}
	if got := migrate.Resources.Requests[corev1.ResourceCPU]; got.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("expected 100m CPU request, got %s", got.String())
	}
	if !hasEnvVar(migrate.Env, "OPENFGA_TIMEOUT") {
		t.Errorf("expected OPENFGA_TIMEOUT in %+v", migrate.Env)
	}
	if got := job.Spec.Template.Annotations[AnnotationMigrationNonce]; got != "secret-rotation-2" {
		t.Errorf("expected migration nonce on the Job pod template, got %q", got)
	}
	if got := job.Annotations["admission.example.com/inject"]; got != "enabled" {
		t.Errorf("expected custom Job annotation, got %q", got)
	}
	if got := job.Spec.Template.Annotations["admission.example.com/inject"]; got != "enabled" {
		t.Errorf("expected custom pod annotation, got %q", got)
	}
	if got := job.Labels[LabelComponent]; got != "migration" {
		t.Errorf("operator Job label must take precedence, got %q", got)
	}
	if got := job.Spec.Template.Labels[LabelComponent]; got != "migration" {
		t.Errorf("operator pod label must take precedence, got %q", got)
	}
	if got := job.Spec.Template.Labels["network-policy.example.com/database"]; got != "allowed" {
		t.Errorf("expected custom pod label, got %q", got)
	}
}

func TestReconcile_InvalidMigrationPodConfiguration_ReturnsError(t *testing.T) {
	for _, value := range []string{`not-json`, `[{"name":"proxy","image":"proxy:v2","restartPolcy":"Always"}]`, `[] {}`} {
		t.Run(value, func(t *testing.T) {
			dep := newTestDeployment("openfga/openfga:v1.14.0")
			dep.Annotations[AnnotationMigrationSidecars] = value
			r := newReconciler(t, nil, dep)

			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: deploymentKey}); err == nil {
				t.Fatal("expected invalid migration sidecars to return an error")
			}
		})
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
		{"openfga", "latest"},
		{"ghcr.io/openfga/openfga:v1.14.0", "v1.14.0"},
		{"registry.example.com:5000/openfga/openfga:v1.14.0", "v1.14.0"},
		{"registry.example.com:5000/openfga/openfga", "latest"},
		{"openfga/openfga@sha256:abcdef1234567890", "sha256:abcdef1234567890"},
		{"openfga/openfga:v1.14.0@sha256:abcdef1234567890", "sha256:abcdef1234567890"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			if got := extractImageTag(tt.image); got != tt.expected {
				t.Errorf("extractImageTag(%q) = %q, want %q", tt.image, got, tt.expected)
			}
		})
	}
}

func TestClearMigrationFailedConditionIdempotent(t *testing.T) {
	dep := &appsv1.Deployment{}
	if clearMigrationFailedCondition(dep) {
		t.Error("expected no change when the MigrationFailed condition is absent")
	}
	if len(dep.Status.Conditions) != 0 {
		t.Errorf("expected no conditions to be added, got %d", len(dep.Status.Conditions))
	}

	dep.Status.Conditions = []appsv1.DeploymentCondition{{Type: "MigrationFailed", Status: corev1.ConditionTrue}}
	if !clearMigrationFailedCondition(dep) {
		t.Error("expected a change when clearing a True MigrationFailed condition")
	}
	cond := findCondition(dep.Status.Conditions, "MigrationFailed")
	if cond == nil || cond.Status != corev1.ConditionFalse {
		t.Fatalf("expected MigrationFailed=False after clear, got %+v", cond)
	}
	transition := cond.LastTransitionTime

	if clearMigrationFailedCondition(dep) {
		t.Error("expected no change when the MigrationFailed condition is already False")
	}
	if cond := findCondition(dep.Status.Conditions, "MigrationFailed"); !cond.LastTransitionTime.Equal(&transition) {
		t.Error("LastTransitionTime must not change when the condition is already False")
	}
}

func TestSetMigrationFailedConditionIdempotent(t *testing.T) {
	dep := &appsv1.Deployment{}
	if !setMigrationFailedCondition(dep, "v1.14.0") {
		t.Error("expected a change when setting MigrationFailed on a fresh deployment")
	}
	cond := findCondition(dep.Status.Conditions, "MigrationFailed")
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("expected MigrationFailed=True, got %+v", cond)
	}
	transition := cond.LastTransitionTime

	if setMigrationFailedCondition(dep, "v1.14.0") {
		t.Error("expected no change when re-setting the same MigrationFailed condition")
	}
	if !setMigrationFailedCondition(dep, "v1.15.0") {
		t.Error("expected a change when the failure message changes")
	}
	if cond := findCondition(dep.Status.Conditions, "MigrationFailed"); !cond.LastTransitionTime.Equal(&transition) {
		t.Error("LastTransitionTime must not change without a status transition")
	}
}
