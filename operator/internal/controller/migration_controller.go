package controller

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// MigrationReconciler watches OpenFGA Deployments and orchestrates database
// migrations when the application version changes.
type MigrationReconciler struct {
	client.Client

	// BackoffLimit for migration Jobs.
	BackoffLimit int32
	// ActiveDeadlineSeconds for migration Jobs.
	ActiveDeadlineSeconds int64
	// TTLSecondsAfterFinished is the diagnostic retention for terminal migration Jobs.
	TTLSecondsAfterFinished int32
	// Now supplies wall-clock time for retry and retention deadlines.
	Now func() time.Time
}

const (
	migrationRetryCooldown  = 60 * time.Second
	failureTTLDeletionGrace = 5 * time.Minute
)

// Reconcile handles a single reconciliation for an OpenFGA Deployment.
func (r *MigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := r.now()

	// 1. Get the OpenFGA Deployment.
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Owned-resource watches bypass the Deployment label predicate, so
	// revalidate both discovery labels before any migration state mutation.
	if deployment.Labels[LabelPartOf] != LabelPartOfValue ||
		deployment.Labels[LabelComponent] != LabelComponentValue {
		logger.V(1).Info("deployment does not match OpenFGA controller labels, skipping")
		return ctrl.Result{}, nil
	}

	// 3. Skip if migration is not opted-in via annotation.
	if len(deployment.Annotations) == 0 || deployment.Annotations[AnnotationMigrationEnabled] != "true" {
		logger.V(1).Info("migration not enabled for this deployment, skipping")
		return ctrl.Result{}, nil
	}

	// 4. Find the OpenFGA container and extract the desired version.
	mainContainer, err := findOpenFGAContainer(deployment)
	if err != nil {
		logger.Error(err, "unable to find OpenFGA container")
		return ctrl.Result{}, err
	}
	desiredVersion := extractImageTag(mainContainer.Image)

	// 4b. Skip migration for memory datastore — just ensure the Deployment is scaled up.
	if isMemoryDatastore(mainContainer) {
		logger.V(1).Info("memory datastore detected, skipping migration")
		if _, scaleErr := ensureDeploymentScaled(ctx, r.Client, deployment); scaleErr != nil {
			return ctrl.Result{}, scaleErr
		}
		return ctrl.Result{}, nil
	}

	// 5. Check current migration status from ConfigMap.
	configMap := &corev1.ConfigMap{}
	cmName := migrationConfigMapName(req.Name)
	err = r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: req.Namespace}, configMap)

	currentVersion := ""
	if err == nil {
		if !isOperatorManaged(configMap) {
			return ctrl.Result{}, migrationStatusConfigMapCollision(configMap)
		}
		if ownershipErr := ensureMigrationStatusOwnership(
			ctx,
			r.Client,
			deployment,
			configMap,
		); ownershipErr != nil {
			return ctrl.Result{}, ownershipErr
		}
		currentVersion = configMap.Data["version"]
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("getting migration status: %w", err)
	}

	// 6. If versions match, repair status ownership, finish terminal cleanup,
	// ensure the Deployment is scaled up, and return without rerunning migration.
	if currentVersion == desiredVersion {
		logger.V(1).Info("migration up to date", "version", desiredVersion)
		if result, handled, jobErr := r.reconcileCurrentVersionJob(
			ctx,
			deployment,
			desiredVersion,
			now,
		); handled || jobErr != nil {
			return result, jobErr
		}
		statusPatch := client.MergeFromWithOptions(
			deployment.DeepCopy(),
			client.MergeFromWithOptimisticLock{},
		)
		if clearMigrationFailedCondition(deployment, now) {
			if patchErr := r.Status().Patch(ctx, deployment, statusPatch); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("clearing MigrationFailed condition: %w", patchErr)
			}
		}
		if _, scaleErr := ensureDeploymentScaled(ctx, r.Client, deployment); scaleErr != nil {
			return ctrl.Result{}, scaleErr
		}
		return ctrl.Result{}, nil
	}

	logger.Info("migration needed", "currentVersion", currentVersion, "desiredVersion", desiredVersion)

	// 7. Check if a migration Job already exists.
	jobName := migrationJobName(req.Name)
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: req.Namespace}, job)

	if apierrors.IsNotFound(err) {
		if retryTime, ok := deploymentRetryAfter(deployment); ok && now.Before(retryTime) {
			remaining := retryTime.Sub(now)
			logger.V(1).Info(
				"in retry cooldown",
				"retryAfter",
				retryTime.Format(time.RFC3339),
				"remaining",
				remaining,
			)
			return ctrl.Result{RequeueAfter: remaining}, nil
		}

		// Create the migration Job.
		job = buildMigrationJob(
			deployment,
			mainContainer,
			desiredVersion,
			r.BackoffLimit,
			r.ActiveDeadlineSeconds,
		)
		if createErr := r.Create(ctx, job); createErr != nil {
			if apierrors.IsAlreadyExists(createErr) {
				// A concurrent reconcile already created the Job; requeue to pick it up.
				logger.V(1).Info("migration job already exists, will recheck", "job", jobName)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			// Leave the retry-after annotation intact so the cooldown survives this failure.
			return ctrl.Result{}, fmt.Errorf("creating migration job: %w", createErr)
		}
		// Clear the retry-after annotation now that the Job is created.
		if _, hasRetry := deployment.Annotations[AnnotationRetryAfter]; hasRetry {
			patch := client.MergeFromWithOptions(
				deployment.DeepCopy(),
				client.MergeFromWithOptimisticLock{},
			)
			delete(deployment.Annotations, AnnotationRetryAfter)
			if patchErr := r.Patch(ctx, deployment, patch); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("clearing retry-after annotation: %w", patchErr)
			}
		}
		logger.Info("created migration job", "job", jobName, "version", desiredVersion)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting migration job: %w", err)
	}

	if job.DeletionTimestamp != nil {
		logger.V(1).Info("waiting for migration job deletion to complete", "job", jobName)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if !isOperatorManaged(job) || !isOwnedByDeployment(job, deployment) {
		if isLegacyHelmMigrationHook(job, deployment) {
			// This exact Job belongs to the same Helm release and uses the chart's
			// migration hook lifecycle, so removing it is safe during operator adoption.
			logger.Info("deleting legacy Helm migration hook before operator adoption", "job", jobName)
			if delErr := deleteMigrationJob(ctx, r.Client, job); delErr != nil && !apierrors.IsNotFound(delErr) {
				if apierrors.IsConflict(delErr) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return ctrl.Result{}, fmt.Errorf("deleting legacy Helm migration job: %w", delErr)
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf(
			"migration job %s/%s exists but must be managed by %s and owned by Deployment %s",
			job.Namespace,
			job.Name,
			LabelManagedByValue,
			deployment.Name,
		)
	}

	// Jobs created by older operator versions may already have a TTL. Remove it
	// before inspecting an unrecorded result so the TTL controller cannot race
	// result persistence. Continue in this reconcile to minimize any legacy race.
	if job.Spec.TTLSecondsAfterFinished != nil &&
		job.Annotations[AnnotationRetainUntil] == "" {
		if _, disarmErr := disarmMigrationJobTTL(ctx, r.Client, job); disarmErr != nil {
			return ctrl.Result{}, disarmErr
		}
	}

	// A recorded failure owns its retention lifecycle even if the Deployment
	// image changes while diagnostics are being retained.
	if isJobConditionTrue(job, batchv1.JobFailed) ||
		isJobConditionTrue(job, batchv1.JobFailureTarget) {
		failedVersion := migrationJobRecordedVersion(job)
		if failedVersion == "" {
			failedVersion = "unknown"
		}
		return r.reconcileFailedMigrationJob(ctx, deployment, job, failedVersion, now)
	}

	// 8. If the existing Job is for a different (or unknown) version, delete it
	// and recreate. Check annotation first (supports digests > 63 chars), fall
	// back to label. A Job with neither marker is treated as stale: we cannot
	// trust its outcome to represent the current desired version, so trusting
	// JobComplete in step 9 would write a wrong version into the status ConfigMap.
	if !migrationJobVersionMatches(job, desiredVersion) {
		jobVersion := job.Annotations[AnnotationDesiredVersion]
		if jobVersion == "" {
			jobVersion = job.Labels["app.kubernetes.io/version"]
		}
		logger.Info("existing migration job is for a different or unknown version, deleting", "jobVersion", jobVersion, "desiredVersion", desiredVersion)
		if delErr := deleteMigrationJob(ctx, r.Client, job); delErr != nil && !apierrors.IsNotFound(delErr) {
			if apierrors.IsConflict(delErr) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, fmt.Errorf("deleting stale migration job: %w", delErr)
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// 9. Check Job status using conditions for authoritative completion signals.
	if isJobConditionTrue(job, batchv1.JobComplete) {
		logger.Info("migration succeeded", "version", desiredVersion)

		// Clear MigrationFailed condition.
		statusPatch := client.MergeFromWithOptions(
			deployment.DeepCopy(),
			client.MergeFromWithOptimisticLock{},
		)
		if clearMigrationFailedCondition(deployment, now) {
			if patchErr := r.Status().Patch(ctx, deployment, statusPatch); patchErr != nil {
				return ctrl.Result{}, fmt.Errorf("clearing MigrationFailed condition: %w", patchErr)
			}
		}

		// Update migration status ConfigMap.
		if statusErr := updateMigrationStatus(
			ctx,
			r.Client,
			deployment,
			desiredVersion,
			jobName,
			now,
		); statusErr != nil {
			return ctrl.Result{}, statusErr
		}

		// A terminal Job becomes TTL-eligible only after its result is durable.
		if ttlErr := armMigrationJobTTL(
			ctx,
			r.Client,
			job,
			r.TTLSecondsAfterFinished,
			nil,
		); ttlErr != nil {
			return ctrl.Result{}, ttlErr
		}

		// Scale Deployment back up.
		if _, scaleErr := ensureDeploymentScaled(ctx, r.Client, deployment); scaleErr != nil {
			return ctrl.Result{}, scaleErr
		}

		return ctrl.Result{}, nil
	}

	if _, hasRetry := deployment.Annotations[AnnotationRetryAfter]; hasRetry {
		if clearErr := clearDeploymentRetryAfter(ctx, r.Client, deployment); clearErr != nil {
			return ctrl.Result{}, clearErr
		}
	}

	// 10. Job still running — requeue.
	logger.V(1).Info("migration job in progress", "job", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *MigrationReconciler) reconcileCurrentVersionJob(
	ctx context.Context,
	deployment *appsv1.Deployment,
	desiredVersion string,
	now time.Time,
) (ctrl.Result, bool, error) {
	job := &batchv1.Job{}
	if err := r.Get(
		ctx,
		types.NamespacedName{
			Name:      migrationJobName(deployment.Name),
			Namespace: deployment.Namespace,
		},
		job,
	); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, false, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("getting retained migration Job: %w", err)
	}
	if job.DeletionTimestamp != nil {
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}
	if !isOperatorManaged(job) || !isOwnedByDeployment(job, deployment) {
		return ctrl.Result{}, true, fmt.Errorf(
			"migration job %s/%s exists but must be managed by %s and controlled by Deployment %s",
			job.Namespace,
			job.Name,
			LabelManagedByValue,
			deployment.Name,
		)
	}

	if isJobConditionTrue(job, batchv1.JobFailed) ||
		isJobConditionTrue(job, batchv1.JobFailureTarget) {
		if job.Spec.TTLSecondsAfterFinished != nil &&
			job.Annotations[AnnotationRetainUntil] == "" {
			if _, err := disarmMigrationJobTTL(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, true, err
			}
		}

		failedVersion := migrationJobRecordedVersion(job)
		if failedVersion == "" {
			failedVersion = "unknown"
		}
		result, err := r.reconcileFailedMigrationJob(
			ctx,
			deployment,
			job,
			failedVersion,
			now,
		)
		return result, true, err
	}

	if !migrationJobVersionMatches(job, desiredVersion) ||
		!isJobConditionTrue(job, batchv1.JobComplete) {
		if err := deleteMigrationJob(ctx, r.Client, job); err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, true, nil
			}
			return ctrl.Result{}, true, fmt.Errorf(
				"deleting conflicting migration job for current status: %w",
				err,
			)
		}
		return ctrl.Result{RequeueAfter: time.Second}, true, nil
	}

	if err := armMigrationJobTTL(
		ctx,
		r.Client,
		job,
		r.TTLSecondsAfterFinished,
		nil,
	); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{}, false, nil
}

func (r *MigrationReconciler) reconcileFailedMigrationJob(
	ctx context.Context,
	deployment *appsv1.Deployment,
	job *batchv1.Job,
	failedVersion string,
	now time.Time,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info(
		"migration job failed, retaining diagnostics before retry",
		"job",
		job.Name,
		"version",
		failedVersion,
	)

	statusPatch := client.MergeFromWithOptions(
		deployment.DeepCopy(),
		client.MergeFromWithOptimisticLock{},
	)
	if setMigrationFailedCondition(deployment, failedVersion, now) {
		if err := r.Status().Patch(ctx, deployment, statusPatch); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting MigrationFailed condition: %w", err)
		}
	}

	retryAfter, retainUntil, recorded, err := recordedFailureDeadlines(job)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !recorded {
		if existingRetry, ok := deploymentRetryAfter(deployment); ok && now.Before(existingRetry) {
			retryAfter = existingRetry
		} else {
			retryAfter = now.Add(migrationRetryCooldown)
		}
		retainUntil = failureRetentionDeadline(
			job,
			retryAfter,
			now,
			r.TTLSecondsAfterFinished,
		)
	} else {
		extendedDeadline := failureRetentionDeadline(
			job,
			retryAfter,
			now,
			r.TTLSecondsAfterFinished,
		)
		if extendedDeadline.After(retainUntil) {
			retainUntil = extendedDeadline
		}
	}

	if err := ensureDeploymentRetryAfter(ctx, r.Client, deployment, retryAfter); err != nil {
		return ctrl.Result{}, err
	}

	effectiveTTL, err := failureTTLSeconds(
		job,
		retryAfter,
		retainUntil,
		now,
		r.TTLSecondsAfterFinished,
	)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := armMigrationJobTTL(
		ctx,
		r.Client,
		job,
		effectiveTTL,
		map[string]string{
			AnnotationRetryAfter:  retryAfter.Format(time.RFC3339),
			AnnotationRetainUntil: retainUntil.Format(time.RFC3339),
		},
	); err != nil {
		return ctrl.Result{}, err
	}

	// FailureTarget surfaces the error early, but the diagnostic window starts
	// only after JobFailed confirms that all Pods have finished terminating.
	if !isJobConditionTrue(job, batchv1.JobFailed) {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if now.Before(retainUntil) {
		return ctrl.Result{RequeueAfter: retainUntil.Sub(now)}, nil
	}

	if err := deleteMigrationJob(ctx, r.Client, job); err != nil && !apierrors.IsNotFound(err) {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("deleting failed migration job: %w", err)
	}
	logger.Info("deleting retained failed migration job before retry", "job", job.Name)
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

// isJobConditionTrue returns true if the Job has a condition of the given type
// with status True. This is more reliable than comparing status counters because
// the Job controller sets conditions atomically when it makes its final decision.
func isJobConditionTrue(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == conditionType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isMemoryDatastore checks if the Deployment is using the memory datastore
// (no database migration needed).
//
// NOTE: This only inspects literal values in explicit env vars. If
// OPENFGA_DATASTORE_ENGINE is injected via envFrom or resolved via valueFrom,
// its value is unknown here and the operator will attempt a migration.
func isMemoryDatastore(container *corev1.Container) bool {
	for _, env := range container.Env {
		if env.Name == "OPENFGA_DATASTORE_ENGINE" {
			return strings.EqualFold(env.Value, "memory")
		}
	}
	return false
}

// setMigrationFailedCondition sets a MigrationFailed condition on the Deployment.
func setMigrationFailedCondition(
	deployment *appsv1.Deployment,
	version string,
	now time.Time,
) bool {
	message := fmt.Sprintf("Database migration failed for version %s. Check migration job logs.", version)
	condition := appsv1.DeploymentCondition{
		Type:               "MigrationFailed",
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(now),
		Reason:             "MigrationJobFailed",
		Message:            message,
	}

	// Replace existing MigrationFailed condition if present.
	for i, c := range deployment.Status.Conditions {
		if c.Type == "MigrationFailed" {
			if c.Status == corev1.ConditionTrue &&
				c.Reason == condition.Reason &&
				c.Message == message {
				return false
			}
			deployment.Status.Conditions[i] = condition
			return true
		}
	}
	deployment.Status.Conditions = append(deployment.Status.Conditions, condition)
	return true
}

// clearMigrationFailedCondition sets the MigrationFailed condition to False and
// reports whether the status changed.
func clearMigrationFailedCondition(deployment *appsv1.Deployment, now time.Time) bool {
	const (
		reason  = "MigrationSucceeded"
		message = "Migration completed successfully."
	)

	for i, c := range deployment.Status.Conditions {
		if c.Type == "MigrationFailed" {
			if c.Status == corev1.ConditionFalse && c.Reason == reason && c.Message == message {
				return false
			}
			deployment.Status.Conditions[i].Status = corev1.ConditionFalse
			deployment.Status.Conditions[i].LastTransitionTime = metav1.NewTime(now)
			deployment.Status.Conditions[i].Reason = reason
			deployment.Status.Conditions[i].Message = message
			return true
		}
	}
	return false
}

func (r *MigrationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC().Truncate(time.Second)
	}
	return time.Now().UTC().Truncate(time.Second)
}

func migrationJobVersionMatches(job *batchv1.Job, desiredVersion string) bool {
	if jobVersion := job.Annotations[AnnotationDesiredVersion]; jobVersion != "" {
		return jobVersion == desiredVersion
	}

	sanitized := strings.ReplaceAll(desiredVersion, ":", "_")
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
	}
	jobVersion := job.Labels["app.kubernetes.io/version"]
	return jobVersion != "" && jobVersion == sanitized
}

func migrationJobRecordedVersion(job *batchv1.Job) string {
	if version := job.Annotations[AnnotationDesiredVersion]; version != "" {
		return version
	}
	return job.Labels["app.kubernetes.io/version"]
}

func deploymentRetryAfter(deployment *appsv1.Deployment) (time.Time, bool) {
	value, ok := deployment.Annotations[AnnotationRetryAfter]
	if !ok {
		return time.Time{}, false
	}
	retryAfter, err := time.Parse(time.RFC3339, value)
	return retryAfter, err == nil
}

func ensureDeploymentRetryAfter(
	ctx context.Context,
	c client.Client,
	deployment *appsv1.Deployment,
	retryAfter time.Time,
) error {
	value := retryAfter.UTC().Format(time.RFC3339)
	if deployment.Annotations[AnnotationRetryAfter] == value {
		return nil
	}

	patch := client.MergeFromWithOptions(
		deployment.DeepCopy(),
		client.MergeFromWithOptimisticLock{},
	)
	if deployment.Annotations == nil {
		deployment.Annotations = make(map[string]string)
	}
	deployment.Annotations[AnnotationRetryAfter] = value
	if err := c.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("persisting retry-after annotation: %w", err)
	}
	return nil
}

func clearDeploymentRetryAfter(
	ctx context.Context,
	c client.Client,
	deployment *appsv1.Deployment,
) error {
	if _, ok := deployment.Annotations[AnnotationRetryAfter]; !ok {
		return nil
	}
	patch := client.MergeFromWithOptions(
		deployment.DeepCopy(),
		client.MergeFromWithOptimisticLock{},
	)
	delete(deployment.Annotations, AnnotationRetryAfter)
	if err := c.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("clearing retry-after annotation: %w", err)
	}
	return nil
}

func recordedFailureDeadlines(job *batchv1.Job) (time.Time, time.Time, bool, error) {
	retryValue, hasRetry := job.Annotations[AnnotationRetryAfter]
	retainValue, hasRetain := job.Annotations[AnnotationRetainUntil]
	if !hasRetry && !hasRetain {
		return time.Time{}, time.Time{}, false, nil
	}
	if !hasRetry || !hasRetain {
		return time.Time{}, time.Time{}, false, fmt.Errorf(
			"migration Job %s/%s has incomplete failure retention metadata",
			job.Namespace,
			job.Name,
		)
	}

	retryAfter, retryErr := time.Parse(time.RFC3339, retryValue)
	if retryErr != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf(
			"parsing migration Job retry deadline: %w",
			retryErr,
		)
	}
	retainUntil, retainErr := time.Parse(time.RFC3339, retainValue)
	if retainErr != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf(
			"parsing migration Job retention deadline: %w",
			retainErr,
		)
	}
	return retryAfter, retainUntil, true, nil
}

func failureRetentionDeadline(
	job *batchv1.Job,
	retryAfter time.Time,
	now time.Time,
	diagnosticTTL int32,
) time.Time {
	finishedAt, finished := jobFinishedTime(job)
	if !finished {
		return retryAfter
	}
	retainUntil := finishedAt.Add(time.Duration(diagnosticTTL) * time.Second)
	if retryAfter.After(retainUntil) {
		return retryAfter
	}
	return retainUntil
}

func failureTTLSeconds(
	job *batchv1.Job,
	retryAfter time.Time,
	retainUntil time.Time,
	now time.Time,
	diagnosticTTL int32,
) (int32, error) {
	var seconds int64
	if finishedAt, finished := jobFinishedTime(job); finished {
		seconds = int64(math.Ceil(retainUntil.Sub(finishedAt).Seconds()))
	} else {
		seconds = int64(diagnosticTTL)
		retrySeconds := int64(math.Ceil(retryAfter.Sub(now).Seconds()))
		if retrySeconds > seconds {
			seconds = retrySeconds
		}
	}
	if seconds < 0 {
		seconds = 0
	}
	if seconds > math.MaxInt32 {
		return 0, fmt.Errorf(
			"migration Job retention duration %d seconds exceeds Kubernetes limit",
			seconds,
		)
	}
	// Keep the native TTL as a fallback just beyond the operator's retention
	// deadline so the normal path can issue a foreground, preconditioned delete.
	graceSeconds := int64(failureTTLDeletionGrace / time.Second)
	if seconds <= math.MaxInt32-graceSeconds {
		seconds += graceSeconds
	} else {
		seconds = math.MaxInt32
	}
	return int32(seconds), nil
}

func jobFinishedTime(job *batchv1.Job) (time.Time, bool) {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time.UTC(), true
	}

	finishedAt := time.Time{}
	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue ||
			(condition.Type != batchv1.JobComplete &&
				condition.Type != batchv1.JobFailed) ||
			condition.LastTransitionTime.IsZero() {
			continue
		}
		if condition.LastTransitionTime.Time.After(finishedAt) {
			finishedAt = condition.LastTransitionTime.Time
		}
	}
	if !finishedAt.IsZero() {
		return finishedAt.UTC(), true
	}
	return time.Time{}, false
}

// SetupWithManager sets up the controller with the Manager.
func (r *MigrationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Only watch Deployments that are part of OpenFGA.
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{
			LabelPartOf:    LabelPartOfValue,
			LabelComponent: LabelComponentValue,
		},
	})
	if err != nil {
		return fmt.Errorf("creating label predicate: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(labelPredicate)).
		Owns(&batchv1.Job{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				// Only watch ConfigMaps that are migration status ConfigMaps.
				if obj.GetLabels()[LabelPartOf] != LabelPartOfValue ||
					obj.GetLabels()[LabelManagedBy] != LabelManagedByValue {
					return nil
				}
				// Map back to the owning Deployment.
				for _, ref := range obj.GetOwnerReferences() {
					if ref.Kind == "Deployment" {
						return []reconcile.Request{
							{NamespacedName: types.NamespacedName{
								Name:      ref.Name,
								Namespace: obj.GetNamespace(),
							}},
						}
					}
				}
				return nil
			},
		)).
		Complete(r)
}
