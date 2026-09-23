package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
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
	AnnotationMigrationTrigger        = "openfga.dev/migration-trigger"
	AnnotationMigrationInitContainers = "openfga.dev/migration-init-containers"
	AnnotationMigrationSidecars       = "openfga.dev/migration-sidecars"
	AnnotationMigrationVolumes        = "openfga.dev/migration-volumes"
	AnnotationMigrationVolumeMounts   = "openfga.dev/migration-volume-mounts"
	AnnotationMigrationResources      = "openfga.dev/migration-resources"
	AnnotationMigrationTimeout        = "openfga.dev/migration-timeout"
	AnnotationMigrationAnnotations    = "openfga.dev/migration-annotations"
	AnnotationMigrationLabels         = "openfga.dev/migration-labels"

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
	Version         string
	Trigger         string
	PodTemplateHash string
}

func desiredIdentity(deployment *appsv1.Deployment, container *corev1.Container) migrationIdentity {
	return migrationIdentity{
		Version: extractImageTag(container.Image),
		Trigger: deployment.Annotations[AnnotationMigrationTrigger],
	}
}

func jobIdentity(job *batchv1.Job) migrationIdentity {
	return migrationIdentity{
		Version:         job.Annotations[AnnotationDesiredVersion],
		Trigger:         job.Annotations[AnnotationMigrationTrigger],
		PodTemplateHash: job.Annotations[AnnotationPodTemplateHash],
	}
}

// recordedIdentity returns the identity stored in the status ConfigMap, or the
// zero value when the ConfigMap is missing or not owned by this Deployment.
func recordedIdentity(cm *corev1.ConfigMap, deployment *appsv1.Deployment) migrationIdentity {
	if !metav1.IsControlledBy(cm, deployment) {
		return migrationIdentity{}
	}
	return migrationIdentity{
		Version:         cm.Data["version"],
		Trigger:         cm.Data["trigger"],
		PodTemplateHash: cm.Data["podTemplateHash"],
	}
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

func isOperatorManagedResourceForDeployment(obj metav1.Object, deployment *appsv1.Deployment) bool {
	if obj.GetLabels()[LabelManagedBy] != LabelManagedByValue {
		return false
	}
	for _, owner := range obj.GetOwnerReferences() {
		if ptr.Deref(owner.Controller, false) &&
			owner.APIVersion == "apps/v1" &&
			owner.Kind == "Deployment" &&
			owner.Name == deployment.Name {
			return true
		}
	}
	return false
}

func isLegacyMigrationJob(job *batchv1.Job) bool {
	return job.Labels[LabelManagedBy] == "Helm" && job.Annotations["helm.sh/hook"] != ""
}

// buildMigrationJob constructs a Job that runs "openfga migrate" with the
// OpenFGA container's image and environment, the Deployment's pod scheduling,
// and chart-provided migration-specific configuration. Other Deployment
// containers and migration sidecars run as native sidecars.
func (r *MigrationReconciler) buildMigrationJob(deployment *appsv1.Deployment, container *corev1.Container, desired migrationIdentity) (*batchv1.Job, error) {
	podSpec := deployment.Spec.Template.Spec
	serviceAccount := deployment.Annotations[AnnotationMigrationServiceAccount]
	if serviceAccount == "" {
		serviceAccount = podSpec.ServiceAccountName
	}

	migrationInitContainers, err := annotationJSON[[]corev1.Container](deployment, AnnotationMigrationInitContainers)
	if err != nil {
		return nil, err
	}
	migrationSidecars, err := annotationJSON[[]corev1.Container](deployment, AnnotationMigrationSidecars)
	if err != nil {
		return nil, err
	}
	extraVolumes, err := annotationJSON[[]corev1.Volume](deployment, AnnotationMigrationVolumes)
	if err != nil {
		return nil, err
	}
	extraVolumeMounts, err := annotationJSON[[]corev1.VolumeMount](deployment, AnnotationMigrationVolumeMounts)
	if err != nil {
		return nil, err
	}
	migrationAnnotations, err := annotationJSON[map[string]string](deployment, AnnotationMigrationAnnotations)
	if err != nil {
		return nil, err
	}
	migrationLabels, err := annotationJSON[map[string]string](deployment, AnnotationMigrationLabels)
	if err != nil {
		return nil, err
	}

	resources := container.Resources
	if deployment.Annotations[AnnotationMigrationResources] != "" {
		resources, err = annotationJSON[corev1.ResourceRequirements](deployment, AnnotationMigrationResources)
		if err != nil {
			return nil, err
		}
	}
	env := append([]corev1.EnvVar(nil), container.Env...)
	if timeout := deployment.Annotations[AnnotationMigrationTimeout]; timeout != "" && !hasEnvVar(env, "OPENFGA_TIMEOUT") {
		env = append(env, corev1.EnvVar{Name: "OPENFGA_TIMEOUT", Value: timeout})
	}

	podAnnotations := mergeStringMaps(migrationAnnotations, map[string]string{
		AnnotationMigrationTrigger: desired.Trigger,
	})
	jobLabels := mergeStringMaps(migrationLabels, map[string]string{
		LabelPartOf:    LabelPartOfValue,
		LabelComponent: "migration",
		LabelManagedBy: LabelManagedByValue,
	})
	podLabels := mergeStringMaps(migrationLabels, map[string]string{
		LabelPartOf:    LabelPartOfValue,
		LabelComponent: "migration",
	})
	jobAnnotations := mergeStringMaps(migrationAnnotations, map[string]string{
		AnnotationDesiredVersion:   desired.Version,
		AnnotationMigrationTrigger: desired.Trigger,
	})

	// Native sidecars start before regular init containers and stop when the
	// migration container exits.
	var runtimeSidecars []corev1.Container
	for i := range podSpec.Containers {
		if podSpec.Containers[i].Name == container.Name {
			continue
		}
		sidecar := *podSpec.Containers[i].DeepCopy()
		runtimeSidecars = append(runtimeSidecars, sidecar)
	}
	sidecars := mergeContainers(runtimeSidecars, migrationSidecars)
	var initContainers []corev1.Container
	for i := range sidecars {
		sidecar := *sidecars[i].DeepCopy()
		sidecar.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
		initContainers = append(initContainers, sidecar)
	}
	initContainers = append(initContainers, mergeContainers(podSpec.InitContainers, migrationInitContainers)...)

	containers := []corev1.Container{{
		Name:            "migrate-database",
		Image:           container.Image,
		ImagePullPolicy: container.ImagePullPolicy,
		Args:            []string{"migrate"},
		Env:             env,
		EnvFrom:         container.EnvFrom,
		Resources:       resources,
		VolumeMounts:    mergeVolumeMounts(container.VolumeMounts, extraVolumeMounts),
		SecurityContext: container.SecurityContext,
	}}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            migrationJobName(deployment.Name),
			Namespace:       deployment.Namespace,
			Labels:          jobLabels,
			Annotations:     jobAnnotations,
			OwnerReferences: []metav1.OwnerReference{ownerReference(deployment)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(r.BackoffLimit),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					ImagePullSecrets:   podSpec.ImagePullSecrets,
					SecurityContext:    podSpec.SecurityContext,
					InitContainers:     initContainers,
					Containers:         containers,
					Volumes:            mergeVolumes(podSpec.Volumes, extraVolumes),
					NodeSelector:       podSpec.NodeSelector,
					Tolerations:        podSpec.Tolerations,
					Affinity:           podSpec.Affinity,
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

func annotationJSON[T any](deployment *appsv1.Deployment, annotation string) (T, error) {
	var value T
	raw := deployment.Annotations[annotation]
	if raw == "" {
		return value, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decoding %s annotation on deployment %s/%s: %w", annotation, deployment.Namespace, deployment.Name, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return value, fmt.Errorf("decoding %s annotation on deployment %s/%s: trailing JSON data", annotation, deployment.Namespace, deployment.Name)
	}
	return value, nil
}

func hasEnvVar(env []corev1.EnvVar, name string) bool {
	for i := range env {
		if env[i].Name == name {
			return true
		}
	}
	return false
}

func mergeVolumes(base, extra []corev1.Volume) []corev1.Volume {
	merged := append([]corev1.Volume(nil), base...)
	index := make(map[string]int, len(merged))
	for i := range merged {
		index[merged[i].Name] = i
	}
	for _, volume := range extra {
		if i, ok := index[volume.Name]; ok {
			merged[i] = volume
			continue
		}
		index[volume.Name] = len(merged)
		merged = append(merged, volume)
	}
	return merged
}

func mergeContainers(base, overrides []corev1.Container) []corev1.Container {
	merged := append([]corev1.Container(nil), base...)
	index := make(map[string]int, len(merged))
	for i := range merged {
		index[merged[i].Name] = i
	}
	for _, container := range overrides {
		if i, ok := index[container.Name]; ok {
			merged[i] = container
			continue
		}
		index[container.Name] = len(merged)
		merged = append(merged, container)
	}
	return merged
}

func mergeVolumeMounts(base, extra []corev1.VolumeMount) []corev1.VolumeMount {
	merged := append([]corev1.VolumeMount(nil), base...)
	index := make(map[string]int, len(merged))
	for i := range merged {
		index[merged[i].MountPath] = i
	}
	for _, mount := range extra {
		if i, ok := index[mount.MountPath]; ok {
			merged[i] = mount
			continue
		}
		index[mount.MountPath] = len(merged)
		merged = append(merged, mount)
	}
	return merged
}

func mergeStringMaps(base, overrides map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overrides))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	return merged
}

func podTemplateHash(template *corev1.PodTemplateSpec) string {
	b, err := json.Marshal(template)
	if err != nil {
		panic(err) // a PodTemplateSpec always marshals
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))[:16]
}

// updateMigrationStatus records the migrated identity in the status ConfigMap.
func updateMigrationStatus(ctx context.Context, c client.Client, deployment *appsv1.Deployment, identity migrationIdentity, jobName string) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      migrationConfigMapName(deployment.Name),
		Namespace: deployment.Namespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
		if cm.ResourceVersion != "" &&
			!metav1.IsControlledBy(cm, deployment) &&
			!isOperatorManagedResourceForDeployment(cm, deployment) {
			return fmt.Errorf("ConfigMap %s/%s already exists and is not managed by the OpenFGA operator", cm.Namespace, cm.Name)
		}
		cm.Labels = map[string]string{
			LabelPartOf:    LabelPartOfValue,
			LabelComponent: "migration",
			LabelManagedBy: LabelManagedByValue,
		}
		cm.OwnerReferences = []metav1.OwnerReference{ownerReference(deployment)}
		cm.Data = map[string]string{
			"version":         identity.Version,
			"trigger":         identity.Trigger,
			"podTemplateHash": identity.PodTemplateHash,
			"migratedAt":      time.Now().UTC().Format(time.RFC3339),
			"jobName":         jobName,
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("updating migration status ConfigMap: %w", err)
	}
	return nil
}
