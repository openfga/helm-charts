package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
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
	// AnnotationMigrationTrigger is any string that, together with the image
	// version, identifies a migration. Changing it runs the migration again.
	AnnotationMigrationTrigger = "openfga.dev/migration-trigger"
	// AnnotationMigrationLabels and AnnotationMigrationAnnotations hold JSON
	// maps of metadata to add to the migration Job and its pod.
	AnnotationMigrationLabels      = "openfga.dev/migration-labels"
	AnnotationMigrationAnnotations = "openfga.dev/migration-annotations"

	// Annotations set on migration Jobs.
	AnnotationDesiredVersion  = "openfga.dev/desired-version"
	AnnotationPodTemplateHash = "openfga.dev/pod-template-hash"

	// Defaults for migration Job configuration. An ActiveDeadlineSeconds of 0
	// leaves the Job without a deadline.
	DefaultBackoffLimit            int32 = 3
	DefaultActiveDeadlineSeconds   int64 = 0
	DefaultTTLSecondsAfterFinished int32 = 300
)

// migrationIdentity is what the operator compares to decide whether a
// migration has to run: the OpenFGA image version plus the trigger the chart
// derives from the datastore configuration.
type migrationIdentity struct {
	Version string
	Trigger string
}

func desiredIdentity(deployment *appsv1.Deployment, container *corev1.Container) migrationIdentity {
	return migrationIdentity{
		Version: extractImageTag(container.Image),
		Trigger: deployment.Annotations[AnnotationMigrationTrigger],
	}
}

func jobIdentity(job *batchv1.Job) migrationIdentity {
	return migrationIdentity{
		Version: job.Annotations[AnnotationDesiredVersion],
		Trigger: job.Annotations[AnnotationMigrationTrigger],
	}
}

// recordedIdentity returns the identity stored in the status ConfigMap, or the
// zero value when the ConfigMap is missing or not owned by this Deployment.
func recordedIdentity(cm *corev1.ConfigMap, deployment *appsv1.Deployment) migrationIdentity {
	if !metav1.IsControlledBy(cm, deployment) {
		return migrationIdentity{}
	}
	return migrationIdentity{Version: cm.Data["version"], Trigger: cm.Data["trigger"]}
}

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

// metadataFromAnnotation decodes a JSON map of labels or annotations for the
// migration Job from a Deployment annotation.
func metadataFromAnnotation(deployment *appsv1.Deployment, key string) (map[string]string, error) {
	raw := deployment.Annotations[key]
	if raw == "" {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("parsing annotation %s of deployment %s/%s: %w", key, deployment.Namespace, deployment.Name, err)
	}
	return m, nil
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

// replaceable reports whether the operator may delete an existing Job that
// has the migration Job's name: one it created for this or an earlier
// Deployment, or the chart's legacy Helm hook Job. Anything else belongs to
// someone else.
func replaceable(job *batchv1.Job, deployment *appsv1.Deployment) bool {
	if metav1.IsControlledBy(job, deployment) || job.Labels[LabelManagedBy] == LabelManagedByValue {
		return true
	}
	_, hook := job.Annotations["helm.sh/hook"]
	return hook
}

// buildMigrationJob constructs a Job that runs "openfga migrate" with the
// OpenFGA container's image, environment, volumes and scheduling. The
// Deployment's other containers run as sidecars so a database proxy is
// available to the migration and stops when it exits.
func (r *MigrationReconciler) buildMigrationJob(deployment *appsv1.Deployment, container *corev1.Container, desired migrationIdentity) (*batchv1.Job, error) {
	userLabels, err := metadataFromAnnotation(deployment, AnnotationMigrationLabels)
	if err != nil {
		return nil, err
	}
	userAnnotations, err := metadataFromAnnotation(deployment, AnnotationMigrationAnnotations)
	if err != nil {
		return nil, err
	}

	podSpec := deployment.Spec.Template.Spec
	serviceAccount := deployment.Annotations[AnnotationMigrationServiceAccount]
	if serviceAccount == "" {
		serviceAccount = podSpec.ServiceAccountName
	}

	// Sidecars first so init containers can reach the database through them.
	var initContainers []corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == container.Name {
			continue
		}
		sidecar := *podSpec.Containers[i].DeepCopy()
		sidecar.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		initContainers = append(initContainers, sidecar)
	}
	initContainers = append(initContainers, podSpec.InitContainers...)

	labels := map[string]string{
		LabelPartOf:    LabelPartOfValue,
		LabelComponent: "migration",
		LabelManagedBy: LabelManagedByValue,
	}
	podLabels := maps.Clone(userLabels)
	if podLabels == nil {
		podLabels = map[string]string{}
	}
	maps.Copy(podLabels, labels)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            migrationJobName(deployment.Name),
			Namespace:       deployment.Namespace,
			Labels:          podLabels,
			Annotations:     maps.Clone(userAnnotations),
			OwnerReferences: []metav1.OwnerReference{ownerReference(deployment)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(r.BackoffLimit),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: maps.Clone(userAnnotations),
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets:   podSpec.ImagePullSecrets,
					SecurityContext:    podSpec.SecurityContext,
					InitContainers:     initContainers,
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
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[AnnotationDesiredVersion] = desired.Version
	job.Annotations[AnnotationMigrationTrigger] = desired.Trigger
	job.Annotations[AnnotationPodTemplateHash] = podTemplateHash(&job.Spec.Template)
	return job, nil
}

func podTemplateHash(template *corev1.PodTemplateSpec) string {
	b, err := json.Marshal(template)
	if err != nil {
		panic(err) // a PodTemplateSpec always marshals
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

// updateMigrationStatus records the migrated identity in the status ConfigMap,
// creating it or taking it over from a previous Deployment of the same name.
func updateMigrationStatus(ctx context.Context, c client.Client, deployment *appsv1.Deployment, identity migrationIdentity, jobName string) error {
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
		cm.OwnerReferences = []metav1.OwnerReference{ownerReference(deployment)}
		cm.Data = map[string]string{
			"version":    identity.Version,
			"trigger":    identity.Trigger,
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
