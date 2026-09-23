package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// retryDelay is how long a failed migration Job is kept before it is replaced.
const retryDelay = 60 * time.Second

// MigrationReconciler watches OpenFGA Deployments and runs a database
// migration Job whenever its image or migration inputs change.
type MigrationReconciler struct {
	client.Client

	// BackoffLimit for migration Jobs.
	BackoffLimit int32
	// ActiveDeadlineSeconds for migration Jobs; 0 means no deadline.
	ActiveDeadlineSeconds int64
	// TTLSecondsAfterFinished for migration Jobs.
	TTLSecondsAfterFinished int32
}

// Reconcile handles a single reconciliation for an OpenFGA Deployment.
func (r *MigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if deployment.Annotations[AnnotationMigrationEnabled] != "true" {
		return ctrl.Result{}, nil
	}

	container, err := findOpenFGAContainer(deployment)
	if err != nil {
		return ctrl.Result{}, err
	}
	desiredVersion := extractImageTag(container.Image)
	desiredJob, err := r.buildMigrationJob(deployment, container, desiredVersion)
	if err != nil {
		return ctrl.Result{}, err
	}
	desiredPodTemplateHash := desiredJob.Annotations[AnnotationPodTemplateHash]

	status := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationConfigMapName(req.Name), Namespace: req.Namespace}, status)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("getting migration status: %w", err)
	}
	statusOwnedByDeployment := err == nil && metav1.IsControlledBy(status, deployment)
	if err == nil && !statusOwnedByDeployment && !isOperatorManagedResourceForDeployment(status, deployment) {
		return ctrl.Result{}, fmt.Errorf("migration status ConfigMap %s/%s already exists and is not managed by this Deployment", status.Namespace, status.Name)
	}
	currentVersion := status.Data["version"]
	currentPodTemplateHash := status.Data["podTemplateHash"]
	if statusOwnedByDeployment && currentVersion == desiredVersion && currentPodTemplateHash == desiredPodTemplateHash {
		_, err := r.patchCondition(ctx, deployment, clearMigrationFailedCondition)
		return ctrl.Result{}, err
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationJobName(req.Name), Namespace: req.Namespace}, job)
	if apierrors.IsNotFound(err) {
		job = desiredJob
		if err := r.Create(ctx, job); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The cache has not caught up with a Job created by an earlier reconcile.
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("creating migration job: %w", err)
		}
		logger.Info("created migration job", "job", job.Name, "currentVersion", currentVersion, "desiredVersion", desiredVersion)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting migration job: %w", err)
	}
	jobOwnedByDeployment := metav1.IsControlledBy(job, deployment)
	replaceableJob := jobOwnedByDeployment || isOperatorManagedResourceForDeployment(job, deployment) || isLegacyMigrationJob(job)
	if !replaceableJob {
		return ctrl.Result{}, fmt.Errorf("migration Job %s/%s already exists and is not managed by this Deployment", job.Namespace, job.Name)
	}

	// Only a Job this operator created for the desired migration inputs is trusted.
	// Anything else under the same name, such as a Job for a previous image or
	// the chart's legacy Helm hook Job, is replaced.
	jobVersion := job.Annotations[AnnotationDesiredVersion]
	jobPodTemplateHash := job.Annotations[AnnotationPodTemplateHash]
	complete := isJobConditionTrue(job, batchv1.JobComplete)
	failedAt, failed := jobFailedAt(job)
	started := !complete && !failed && (ptr.Deref(job.Status.Ready, 0) > 0 || job.Status.Succeeded > 0)
	outdated := !jobOwnedByDeployment || jobVersion != desiredVersion || jobPodTemplateHash != desiredPodTemplateHash
	if outdated {
		// Never interrupt a running migration: a non-transactional step such as
		// a concurrent index build that is aborted halfway leaves the schema in
		// a state the next run does not repair. A pod that finished before the
		// Job condition was written is also left alone.
		if started {
			logger.V(1).Info("waiting for started migration job before replacing it", "job", job.Name, "jobVersion", jobVersion, "desiredVersion", desiredVersion)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		logger.Info("replacing migration job", "job", job.Name, "jobVersion", jobVersion, "desiredVersion", desiredVersion)
		if err := r.deleteJob(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if complete {
		if err := r.setCompletedJobTTL(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		if err := updateMigrationStatus(ctx, r.Client, deployment, desiredVersion, desiredPodTemplateHash, job.Name); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("migration succeeded", "version", desiredVersion)
		_, err := r.patchCondition(ctx, deployment, clearMigrationFailedCondition)
		return ctrl.Result{}, err
	}

	if failed {
		changed, err := r.patchCondition(ctx, deployment, func(d *appsv1.Deployment) bool {
			return setMigrationFailedCondition(d, desiredVersion)
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if changed {
			logger.Info("migration job failed", "job", job.Name, "version", desiredVersion, "retryIn", retryDelay)
		}
		// The failed Job itself is the retry timer, so the delay survives
		// operator restarts and leaves the pod logs around to inspect.
		if wait := retryDelay - time.Since(failedAt); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		logger.Info("retrying migration", "job", job.Name, "version", desiredVersion)
		if err := r.deleteJob(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *MigrationReconciler) setCompletedJobTTL(ctx context.Context, job *batchv1.Job) error {
	if job.Spec.TTLSecondsAfterFinished != nil && *job.Spec.TTLSecondsAfterFinished == r.TTLSecondsAfterFinished {
		return nil
	}
	patch := client.MergeFromWithOptions(job.DeepCopy(), client.MergeFromWithOptimisticLock{})
	job.Spec.TTLSecondsAfterFinished = ptr.To(r.TTLSecondsAfterFinished)
	if err := r.Patch(ctx, job, patch); err != nil {
		return fmt.Errorf("setting completed migration Job TTL: %w", err)
	}
	return nil
}

func (r *MigrationReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	options := []client.DeleteOption{client.PropagationPolicy(metav1.DeletePropagationForeground)}
	preconditions := client.Preconditions{}
	hasPreconditions := false
	if job.UID != "" {
		uid := job.UID
		preconditions.UID = &uid
		hasPreconditions = true
	}
	if job.ResourceVersion != "" {
		resourceVersion := job.ResourceVersion
		preconditions.ResourceVersion = &resourceVersion
		hasPreconditions = true
	}
	if hasPreconditions {
		options = append(options, preconditions)
	}
	err := r.Delete(ctx, job, options...)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting migration job %s: %w", job.Name, err)
	}
	return nil
}

// patchCondition applies update to the Deployment's status conditions and
// patches the status only if something changed. The strategic merge patch
// merges conditions by type, so the Deployment controller's own conditions are
// left alone.
func (r *MigrationReconciler) patchCondition(ctx context.Context, deployment *appsv1.Deployment, update func(*appsv1.Deployment) bool) (bool, error) {
	patch := client.StrategicMergeFrom(deployment.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if !update(deployment) {
		return false, nil
	}
	if err := r.Status().Patch(ctx, deployment, patch); err != nil {
		return false, fmt.Errorf("patching MigrationFailed condition: %w", err)
	}
	return true, nil
}

func isJobConditionTrue(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == conditionType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobFailedAt reports whether the Job has failed and when. JobFailureTarget is
// set as soon as the Job controller decides the Job will fail; JobFailed only
// once its pods have terminated.
func jobFailedAt(job *batchv1.Job) (time.Time, bool) {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobFailureTarget || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// setMigrationFailedCondition sets a MigrationFailed condition on the Deployment
// and reports whether anything changed. LastTransitionTime only advances on a
// real status transition.
//
// Deployment.Status.Conditions is []appsv1.DeploymentCondition rather than
// []metav1.Condition, so meta.SetStatusCondition cannot be used.
func setMigrationFailedCondition(deployment *appsv1.Deployment, version string) bool {
	message := fmt.Sprintf("Database migration failed for version %s. Check migration job logs.", version)
	for i, c := range deployment.Status.Conditions {
		if c.Type == "MigrationFailed" {
			if c.Status == corev1.ConditionTrue && c.Reason == "MigrationJobFailed" && c.Message == message {
				return false
			}
			if c.Status != corev1.ConditionTrue {
				deployment.Status.Conditions[i].LastTransitionTime = metav1.Now()
			}
			deployment.Status.Conditions[i].Status = corev1.ConditionTrue
			deployment.Status.Conditions[i].Reason = "MigrationJobFailed"
			deployment.Status.Conditions[i].Message = message
			return true
		}
	}
	deployment.Status.Conditions = append(deployment.Status.Conditions, appsv1.DeploymentCondition{
		Type:               "MigrationFailed",
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "MigrationJobFailed",
		Message:            message,
	})
	return true
}

// clearMigrationFailedCondition sets an existing MigrationFailed condition to
// False and reports whether anything changed. It is a no-op when the condition
// is absent or already False, so healthy Deployments are never re-patched.
func clearMigrationFailedCondition(deployment *appsv1.Deployment) bool {
	for i, c := range deployment.Status.Conditions {
		if c.Type == "MigrationFailed" {
			if c.Status == corev1.ConditionFalse {
				return false
			}
			deployment.Status.Conditions[i].Status = corev1.ConditionFalse
			deployment.Status.Conditions[i].LastTransitionTime = metav1.Now()
			deployment.Status.Conditions[i].Reason = "MigrationSucceeded"
			deployment.Status.Conditions[i].Message = "Migration completed successfully."
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *MigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	openfgaDeployments, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelPartOf:    LabelPartOfValue,
			LabelComponent: LabelComponentValue,
		},
	})
	if err != nil {
		return fmt.Errorf("creating label predicate: %w", err)
	}

	// Owning the status ConfigMap means deleting it triggers a reconcile, which
	// runs the migration again once the previous Job is gone.
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(openfgaDeployments)).
		Owns(&batchv1.Job{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
