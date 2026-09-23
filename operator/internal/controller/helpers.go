package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// Labels used to discover OpenFGA Deployments.
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelComponent = "app.kubernetes.io/component"

	LabelPartOfValue    = "openfga"
	LabelComponentValue = "authorization-controller"

	// Labels set on operator-managed resources (migration Jobs, status ConfigMaps).
	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelManagedByValue = "openfga-operator"

	// Annotations read from the Deployment (set by the Helm chart).
	AnnotationMigrationEnabled        = "openfga.dev/migration-enabled"
	AnnotationContainerName           = "openfga.dev/container-name"
	AnnotationMigrationServiceAccount = "openfga.dev/migration-service-account"

	// Annotations set on migration Jobs: the version the Job migrates to, and a
	// hash of the pod template it was built from.
	AnnotationDesiredVersion  = "openfga.dev/desired-version"
	AnnotationPodTemplateHash = "openfga.dev/pod-template-hash"

	// Defaults for migration Job configuration. An ActiveDeadlineSeconds of 0
	// leaves the Job without a deadline.
	DefaultBackoffLimit            int32 = 3
	DefaultActiveDeadlineSeconds   int64 = 0
	DefaultTTLSecondsAfterFinished int32 = 300
)

// extractImageTag returns the tag portion of a container image reference.
// For "openfga/openfga:v1.14.0" it returns "v1.14.0".
// For "openfga/openfga@sha256:abc..." it returns the digest.
// If there is no tag or digest, it returns "latest".
func extractImageTag(image string) string {
	if idx := strings.LastIndex(image, "@"); idx != -1 {
		return image[idx+1:]
	}

	// Only look for ":" after the last "/" so a registry port is not mistaken for a tag.
	nameAndTag := image[strings.LastIndex(image, "/")+1:]
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

// findOpenFGAContainer returns the container named by the openfga.dev/container-name
// annotation, or the container named "openfga" when the annotation is absent.
func findOpenFGAContainer(deployment *appsv1.Deployment) (*corev1.Container, error) {
	targetName := deployment.Annotations[AnnotationContainerName]
	if targetName == "" {
		targetName = "openfga"
	}
	containers := deployment.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == targetName {
			return &containers[i], nil
		}
	}
	return nil, fmt.Errorf("container %q not found in deployment %s/%s", targetName, deployment.Namespace, deployment.Name)
}

// ownerReference makes the Deployment the controller of a migration Job or
// status ConfigMap so both are garbage collected with it. BlockOwnerDeletion
// is left unset: it needs update on deployments/finalizers, which the
// operator is not granted.
func ownerReference(deployment *appsv1.Deployment) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployment.Name,
		UID:        deployment.UID,
		Controller: ptr.To(true),
	}
}

// buildMigrationJob constructs a Job that runs "openfga migrate" with the
// OpenFGA container's image, environment, volumes and scheduling.
func (r *MigrationReconciler) buildMigrationJob(deployment *appsv1.Deployment, container *corev1.Container, version string) *batchv1.Job {
	podSpec := deployment.Spec.Template.Spec
	serviceAccount := deployment.Annotations[AnnotationMigrationServiceAccount]
	if serviceAccount == "" {
		serviceAccount = podSpec.ServiceAccountName
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      migrationJobName(deployment.Name),
			Namespace: deployment.Namespace,
			Labels: map[string]string{
				LabelPartOf:    LabelPartOfValue,
				LabelComponent: "migration",
				LabelManagedBy: LabelManagedByValue,
			},
			Annotations:     map[string]string{AnnotationDesiredVersion: version},
			OwnerReferences: []metav1.OwnerReference{ownerReference(deployment)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(r.BackoffLimit),
			TTLSecondsAfterFinished: ptr.To(r.TTLSecondsAfterFinished),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelPartOf:    LabelPartOfValue,
						LabelComponent: "migration",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets:   podSpec.ImagePullSecrets,
					SecurityContext:    podSpec.SecurityContext,
					Containers: []corev1.Container{{
						Name:            "migrate-database",
						Image:           container.Image,
						ImagePullPolicy: container.ImagePullPolicy,
						Args:            []string{"migrate"},
						Env:             container.Env,
						EnvFrom:         container.EnvFrom,
						Resources:       container.Resources,
						VolumeMounts:    container.VolumeMounts,
						SecurityContext: container.SecurityContext,
					}},
					Volumes:      podSpec.Volumes,
					NodeSelector: podSpec.NodeSelector,
					Tolerations:  podSpec.Tolerations,
					Affinity:     podSpec.Affinity,
				},
			},
		},
	}
	if r.ActiveDeadlineSeconds > 0 {
		job.Spec.ActiveDeadlineSeconds = ptr.To(r.ActiveDeadlineSeconds)
	}
	job.Annotations[AnnotationPodTemplateHash] = podTemplateHash(&job.Spec.Template)
	return job
}

func podTemplateHash(template *corev1.PodTemplateSpec) string {
	b, err := json.Marshal(template)
	if err != nil {
		panic(err) // a PodTemplateSpec always marshals
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

// updateMigrationStatus records the migrated version in the status ConfigMap.
func updateMigrationStatus(ctx context.Context, c client.Client, deployment *appsv1.Deployment, version, jobName string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      migrationConfigMapName(deployment.Name),
		Namespace: deployment.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		cm.Labels = map[string]string{
			LabelPartOf:    LabelPartOfValue,
			LabelComponent: "migration",
			LabelManagedBy: LabelManagedByValue,
		}
		// Reset on every write in case the Deployment was recreated with a new UID.
		cm.OwnerReferences = []metav1.OwnerReference{ownerReference(deployment)}
		cm.Data = map[string]string{
			"version":    version,
			"migratedAt": time.Now().UTC().Format(time.RFC3339),
			"jobName":    jobName,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("updating migration status ConfigMap: %w", err)
	}
	return nil
}
