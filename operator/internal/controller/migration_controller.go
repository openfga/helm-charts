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
	// TTLSecondsAfterFinished for migration Jobs.
	TTLSecondsAfterFinished int32
}

// Reconcile handles a single reconciliation for an OpenFGA Deployment.
func (r *MigrationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Get the OpenFGA Deployment.
	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, req.NamespacedName, deployment); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Extract the desired version from the Deployment's image tag.
	if len(deployment.Spec.Template.Spec.Containers) == 0 {
		logger.Info("deployment has no containers, skipping")
		return ctrl.Result{}, nil
	}
	desiredVersion := extractImageTag(deployment.Spec.Template.Spec.Containers[0].Image)

	// 3. Check current migration status from ConfigMap.
	configMap := &corev1.ConfigMap{}
	cmName := migrationConfigMapName(req.Name)
	err := r.Get(ctx, types.NamespacedName{Name: cmName, Namespace: req.Namespace}, configMap)

	currentVersion := ""
	if err == nil {
		currentVersion = configMap.Data["version"]
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("getting migration status: %w", err)
	}

	// 4. If versions match, ensure Deployment is scaled up and return.
	if currentVersion == desiredVersion {
		logger.V(1).Info("migration up to date", "version", desiredVersion)
		if _, scaleErr := ensureDeploymentScaled(ctx, r.Client, deployment); scaleErr != nil {
			return ctrl.Result{}, scaleErr
		}
		return ctrl.Result{}, nil
	}

	logger.Info("migration needed", "currentVersion", currentVersion, "desiredVersion", desiredVersion)

	// 5. Ensure the Deployment is scaled to zero before migrating.
	if err := scaleDeploymentToZero(ctx, r.Client, deployment); err != nil {
		return ctrl.Result{}, err
	}

	// 6. Check if a migration Job already exists.
	jobName := migrationJobName(req.Name)
	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: req.Namespace}, job)

	if apierrors.IsNotFound(err) {
		// Create the migration Job.
		job = buildMigrationJob(
			deployment,
			desiredVersion,
			r.BackoffLimit,
			r.ActiveDeadlineSeconds,
			r.TTLSecondsAfterFinished,
		)
		if createErr := r.Create(ctx, job); createErr != nil {
			return ctrl.Result{}, fmt.Errorf("creating migration job: %w", createErr)
		}
		logger.Info("created migration job", "job", jobName, "version", desiredVersion)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting migration job: %w", err)
	}

	// 7. Check Job status.
	if job.Status.Succeeded >= 1 {
		logger.Info("migration succeeded", "version", desiredVersion)

		// Update migration status ConfigMap.
		if statusErr := updateMigrationStatus(ctx, r.Client, deployment, desiredVersion, jobName); statusErr != nil {
			return ctrl.Result{}, statusErr
		}

		// Scale Deployment back up.
		if _, scaleErr := ensureDeploymentScaled(ctx, r.Client, deployment); scaleErr != nil {
			return ctrl.Result{}, scaleErr
		}

		return ctrl.Result{}, nil
	}

	backoffLimit := r.BackoffLimit
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}

	if job.Status.Failed >= backoffLimit {
		logger.Error(nil, "migration job failed, will delete and retry", "job", jobName, "version", desiredVersion)

		// Set condition so kubectl describe shows the failure.
		setMigrationFailedCondition(deployment, desiredVersion)
		if patchErr := r.Status().Update(ctx, deployment); patchErr != nil {
			logger.Error(patchErr, "failed to set MigrationFailed condition")
		}

		// Delete the failed Job so a fresh one is created on the next reconcile.
		// This allows auto-recovery when the database comes back.
		propagation := metav1.DeletePropagationBackground
		if delErr := r.Delete(ctx, job, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); delErr != nil && !apierrors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("deleting failed migration job: %w", delErr)
		}
		logger.Info("deleted failed migration job, will retry", "job", jobName)

		// Requeue after a longer delay to avoid tight retry loops.
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	// 8. Job still running — requeue.
	logger.V(1).Info("migration job in progress", "job", jobName)
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// setMigrationFailedCondition sets a MigrationFailed condition on the Deployment.
func setMigrationFailedCondition(deployment *appsv1.Deployment, version string) {
	condition := appsv1.DeploymentCondition{
		Type:               "MigrationFailed",
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             "MigrationJobFailed",
		Message:            fmt.Sprintf("Database migration failed for version %s. Check migration job logs.", version),
	}

	// Replace existing MigrationFailed condition if present.
	for i, c := range deployment.Status.Conditions {
		if c.Type == "MigrationFailed" {
			deployment.Status.Conditions[i] = condition
			return
		}
	}
	deployment.Status.Conditions = append(deployment.Status.Conditions, condition)
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
					obj.GetLabels()["app.kubernetes.io/managed-by"] != "openfga-operator" {
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
