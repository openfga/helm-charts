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
// migration Job whenever the OpenFGA image version changes.
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

	status := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationConfigMapName(req.Name), Namespace: req.Namespace}, status)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("getting migration status: %w", err)
	}
	currentVersion := status.Data["version"]
	if currentVersion == desiredVersion {
		_, err := r.patchCondition(ctx, deployment, clearMigrationFailedCondition)
		return ctrl.Result{}, err
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationJobName(req.Name), Namespace: req.Namespace}, job)
	if apierrors.IsNotFound(err) {
		job = r.buildMigrationJob(deployment, container, desiredVersion)
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

	// Only a Job this operator created for the desired version is trusted.
	// Anything else under the same name, such as a Job for a previous image or
	// the chart's legacy Helm hook Job, is replaced.
	jobVersion := job.Annotations[AnnotationDesiredVersion]
	complete := isJobConditionTrue(job, batchv1.JobComplete)
	failedAt, failed := jobFailedAt(job)
	outdated := jobVersion != desiredVersion
	// A Job whose pod cannot start (a bad secret reference, an image pull
	// error, an unschedulable pod) never fails on its own, so rebuild it once
	// the Deployment's pod template has changed. A Job with a ready pod is left
	// alone so a running migration is not cut off; one whose pod has just
	// finished may still be rebuilt, which only re-runs a no-op migration.
	if !outdated && !complete && !failed && job.Status.Active > 0 && ptr.Deref(job.Status.Ready, 0) == 0 {
		want := r.buildMigrationJob(deployment, container, desiredVersion)
		outdated = job.Annotations[AnnotationPodTemplateHash] != want.Annotations[AnnotationPodTemplateHash]
	}
	if outdated {
		logger.Info("replacing migration job", "job", job.Name, "jobVersion", jobVersion, "desiredVersion", desiredVersion)
		if err := r.deleteJob(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	if complete {
		if err := updateMigrationStatus(ctx, r.Client, deployment, desiredVersion, job.Name); err != nil {
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

func (r *MigrationReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
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
	patch := client.StrategicMergeFrom(deployment.DeepCopy())
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
