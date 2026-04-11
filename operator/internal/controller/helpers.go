package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Labels used to discover OpenFGA Deployments.
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelComponent = "app.kubernetes.io/component"

	LabelPartOfValue    = "openfga"
	LabelComponentValue = "authorization-controller"

	// Annotations set on the Deployment by the Helm chart / operator.
	AnnotationDesiredReplicas         = "openfga.dev/desired-replicas"
	AnnotationMigrationServiceAccount = "openfga.dev/migration-service-account"
	AnnotationRetryAfter              = "openfga.dev/migration-retry-after"

	// Defaults for migration Job configuration.
	DefaultBackoffLimit            int32 = 3
	DefaultActiveDeadlineSeconds   int64 = 300
	DefaultTTLSecondsAfterFinished int32 = 300
)

// extractImageTag returns the tag portion of a container image reference.
// For "openfga/openfga:v1.14.0" it returns "v1.14.0".
// For "openfga/openfga@sha256:abc..." it returns the digest.
// If there is no tag or digest, it returns "latest".
func extractImageTag(image string) string {
	// Handle digest references.
	if idx := strings.LastIndex(image, "@"); idx != -1 {
		return image[idx+1:]
	}

	// Handle tag references — be careful not to split on the port in a registry URL.
	// Find the last '/' to isolate the image name from the registry.
	lastSlash := strings.LastIndex(image, "/")
	nameAndTag := image
	if lastSlash != -1 {
		nameAndTag = image[lastSlash+1:]
	}

	if idx := strings.LastIndex(nameAndTag, ":"); idx != -1 {
		return nameAndTag[idx+1:]
	}

	return "latest"
}

// migrationConfigMapName returns the name of the ConfigMap used to track migration state.
func migrationConfigMapName(deploymentName string) string {
	return deploymentName + "-migration-status"
}

// migrationJobName returns the name of the migration Job.
func migrationJobName(deploymentName string) string {
	return deploymentName + "-migrate"
}

// findOpenFGAContainer finds the OpenFGA container in the Deployment's pod spec.
// It looks for a container named "openfga" first, then falls back to the first container.
func findOpenFGAContainer(deployment *appsv1.Deployment) *corev1.Container {
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == "openfga" {
			return &deployment.Spec.Template.Spec.Containers[i]
		}
	}
	// Fallback: use the first container (for charts that don't name it "openfga").
	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		return &deployment.Spec.Template.Spec.Containers[0]
	}
	return nil
}

// buildMigrationJob constructs a migration Job for the given Deployment.
func buildMigrationJob(
	deployment *appsv1.Deployment,
	mainContainer *corev1.Container,
	desiredVersion string,
	backoffLimit int32,
	activeDeadlineSeconds int64,
	ttlSecondsAfterFinished int32,
) *batchv1.Job {
	// Determine the migration service account.
	migrationSA := deployment.Annotations[AnnotationMigrationServiceAccount]
	if migrationSA == "" {
		migrationSA = deployment.Spec.Template.Spec.ServiceAccountName
	}

	// Filter env vars — only pass datastore-related vars to the migration Job.
	var datastoreEnvVars []corev1.EnvVar
	for _, env := range mainContainer.Env {
		if strings.HasPrefix(env.Name, "OPENFGA_DATASTORE_") {
			datastoreEnvVars = append(datastoreEnvVars, env)
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationJobName(deployment.Name),
			Namespace: deployment.Namespace,
			Labels: map[string]string{
				LabelPartOf:    LabelPartOfValue,
				LabelComponent: "migration",
				"app.kubernetes.io/managed-by": "openfga-operator",
				"app.kubernetes.io/version":    desiredVersion,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "Deployment",
					Name:               deployment.Name,
					UID:                deployment.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(backoffLimit),
			ActiveDeadlineSeconds:   ptr.To(activeDeadlineSeconds),
			TTLSecondsAfterFinished: ptr.To(ttlSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelPartOf:    LabelPartOfValue,
						LabelComponent: "migration",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: migrationSA,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets:   deployment.Spec.Template.Spec.ImagePullSecrets,
					SecurityContext:    deployment.Spec.Template.Spec.SecurityContext,
					Containers: []corev1.Container{
						{
							Name:            "migrate-database",
							Image:           mainContainer.Image,
							Args:            []string{"migrate"},
							Env:             datastoreEnvVars,
							SecurityContext: mainContainer.SecurityContext,
						},
					},
					// Inherit scheduling constraints from the parent Deployment.
					NodeSelector: deployment.Spec.Template.Spec.NodeSelector,
					Tolerations:  deployment.Spec.Template.Spec.Tolerations,
					Affinity:     deployment.Spec.Template.Spec.Affinity,
				},
			},
		},
	}
}

// updateMigrationStatus creates or updates the migration-status ConfigMap.
func updateMigrationStatus(
	ctx context.Context,
	c client.Client,
	deployment *appsv1.Deployment,
	version string,
	jobName string,
) error {
	cmName := migrationConfigMapName(deployment.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: deployment.Namespace,
			Labels: map[string]string{
				LabelPartOf:    LabelPartOfValue,
				LabelComponent: "migration",
				"app.kubernetes.io/managed-by": "openfga-operator",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "apps/v1",
					Kind:               "Deployment",
					Name:               deployment.Name,
					UID:                deployment.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(true),
				},
			},
		},
		Data: map[string]string{
			"version":    version,
			"migratedAt": time.Now().UTC().Format(time.RFC3339),
			"jobName":    jobName,
		},
	}

	// Try to get existing ConfigMap first.
	existing := &corev1.ConfigMap{}
	err := c.Get(ctx, client.ObjectKeyFromObject(cm), existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("getting migration status ConfigMap: %w", err)
		}
		// ConfigMap doesn't exist — create it.
		if createErr := c.Create(ctx, cm); createErr != nil {
			return fmt.Errorf("creating migration status ConfigMap: %w", createErr)
		}
		return nil
	}

	// Update existing ConfigMap.
	existing.Data = cm.Data
	existing.Labels = cm.Labels
	if updateErr := c.Update(ctx, existing); updateErr != nil {
		return fmt.Errorf("updating migration status ConfigMap: %w", updateErr)
	}
	return nil
}

// ensureDeploymentScaled ensures the Deployment is scaled to the desired replica count.
// The desired count is read from the AnnotationDesiredReplicas annotation.
// Returns true if the Deployment was already at the desired scale.
func ensureDeploymentScaled(ctx context.Context, c client.Client, deployment *appsv1.Deployment) (bool, error) {
	desiredStr, ok := deployment.Annotations[AnnotationDesiredReplicas]
	if !ok || desiredStr == "" {
		// No annotation — nothing to do. The Deployment may not have been scaled down yet.
		return true, nil
	}

	desired, err := strconv.ParseInt(desiredStr, 10, 32)
	if err != nil {
		return false, fmt.Errorf("parsing desired replicas annotation: %w", err)
	}

	desiredInt32 := int32(desired)
	current := int32(1)
	if deployment.Spec.Replicas != nil {
		current = *deployment.Spec.Replicas
	}

	if current == desiredInt32 {
		return true, nil
	}

	patch := client.MergeFrom(deployment.DeepCopy())
	deployment.Spec.Replicas = ptr.To(desiredInt32)
	if patchErr := c.Patch(ctx, deployment, patch); patchErr != nil {
		return false, fmt.Errorf("scaling deployment to %d replicas: %w", desiredInt32, patchErr)
	}
	return false, nil
}

// scaleDeploymentToZero scales the Deployment to 0 replicas, storing the current
// desired count in an annotation so it can be restored later.
func scaleDeploymentToZero(ctx context.Context, c client.Client, deployment *appsv1.Deployment) error {
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		return nil // Already at zero.
	}

	patch := client.MergeFrom(deployment.DeepCopy())

	// Store the current desired replica count before zeroing.
	currentReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		currentReplicas = *deployment.Spec.Replicas
	}

	// Only store if not already stored (avoid overwriting with 0 on re-reconciliation).
	if _, ok := deployment.Annotations[AnnotationDesiredReplicas]; !ok {
		if deployment.Annotations == nil {
			deployment.Annotations = make(map[string]string)
		}
		deployment.Annotations[AnnotationDesiredReplicas] = strconv.FormatInt(int64(currentReplicas), 10)
	}

	deployment.Spec.Replicas = ptr.To(int32(0))

	if err := c.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("scaling deployment to 0: %w", err)
	}
	return nil
}
