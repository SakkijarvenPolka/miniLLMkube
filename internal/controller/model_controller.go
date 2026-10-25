/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	inferencePort    = 8080
	defaultImage     = "ghcr.io/ggml-org/llama.cpp:server"
	downloaderImage  = "curlimages/curl:8.11.1"
	defaultContext   = 4096
	progressDeadline = 1800
	readyCondition   = "Ready"
	modelCacheEnv    = "MODEL_CACHE_HOST_PATH"
	modelVolumeName  = "model"
	inferenceAppName = "minillmkube-inference"

	phasePending   = "Pending"
	phaseDeploying = "Deploying"
	phaseReady     = "Ready"
	phaseFailed    = "Failed"
)

type rolloutState struct {
	phase    string
	status   metav1.ConditionStatus
	reason   string
	message  string
	endpoint string
}

// ModelReconciler reconciles a Model object
type ModelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=llm.example.io,resources=models,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llm.example.io,resources=models/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llm.example.io,resources=models/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.3/pkg/reconcile
func (r *ModelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	model := &llmv1alpha1.Model{}
	if err := r.Get(ctx, req.NamespacedName, model); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := validateSpec(model); err != nil {
		state := rolloutState{
			phase:   phaseFailed,
			status:  metav1.ConditionFalse,
			reason:  "InvalidSpec",
			message: err.Error(),
		}
		return ctrl.Result{}, r.updateStatus(ctx, model, state, 0)
	}

	deployment, err := r.reconcileDeployment(ctx, model)
	if err != nil {
		log.Error(err, "Failed to reconcile Deployment", "name", model.Name)
		statusErr := r.updateStatus(ctx, model, rolloutState{
			phase:   phaseFailed,
			status:  metav1.ConditionFalse,
			reason:  "DeploymentReconcileFailed",
			message: err.Error(),
		}, 0)
		return ctrl.Result{}, errors.Join(err, statusErr)
	}

	if err := r.reconcileService(ctx, model); err != nil {
		log.Error(err, "Failed to reconcile Service", "name", model.Name)
		statusErr := r.updateStatus(ctx, model, rolloutState{
			phase:   phaseFailed,
			status:  metav1.ConditionFalse,
			reason:  "ServiceReconcileFailed",
			message: err.Error(),
		}, deployment.Status.ReadyReplicas)
		return ctrl.Result{}, errors.Join(err, statusErr)
	}

	desiredReplicas := replicasFor(model)
	state := evaluateRollout(deployment, desiredReplicas)
	state.endpoint = endpointFor(model)

	if err := r.updateStatus(ctx, model, state, deployment.Status.ReadyReplicas); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&llmv1alpha1.Model{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Named("model").
		Complete(r)
}

func (r *ModelReconciler) reconcileDeployment(
	ctx context.Context,
	model *llmv1alpha1.Model,
) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		labels := labelsFor(model)
		deployment.Labels = labels
		deployment.Spec.Replicas = pointerTo(replicasFor(model))
		deployment.Spec.ProgressDeadlineSeconds = pointerTo(int32(progressDeadline))
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: labels}
		deployment.Spec.Template.ObjectMeta.Labels = labels
		// Only the fields this operator owns are assigned; see applyPodSpec.
		applyPodSpec(&deployment.Spec.Template.Spec, buildPodSpec(model))
		return controllerutil.SetControllerReference(model, deployment, r.Scheme)
	})
	return deployment, err
}

func (r *ModelReconciler) reconcileService(ctx context.Context, model *llmv1alpha1.Model) error {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      model.Name,
			Namespace: model.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = labelsFor(model)
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Selector = labelsFor(model)
		service.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       inferencePort,
			NodePort:   0,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return controllerutil.SetControllerReference(model, service, r.Scheme)
	})
	return err
}

func (r *ModelReconciler) updateStatus(
	ctx context.Context,
	model *llmv1alpha1.Model,
	state rolloutState,
	readyReplicas int32,
) error {
	before := model.DeepCopy()
	model.Status.Phase = state.phase
	if state.endpoint != "" {
		model.Status.Endpoint = state.endpoint
	}
	model.Status.ReadyReplicas = readyReplicas
	model.Status.CacheKey = cacheKeyFor(model.Spec.Source)
	model.Status.ObservedGeneration = model.Generation
	meta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               readyCondition,
		Status:             state.status,
		ObservedGeneration: model.Generation,
		Reason:             state.reason,
		Message:            state.message,
	})

	if reflect.DeepEqual(before.Status, model.Status) {
		return nil
	}
	return r.Status().Patch(ctx, model, client.MergeFrom(before))
}

func validateSpec(model *llmv1alpha1.Model) error {
	sourceURL, err := url.Parse(model.Spec.Source)
	if err != nil {
		return fmt.Errorf("source must be a valid URL: %w", err)
	}
	if sourceURL.Scheme != "https" {
		return fmt.Errorf("source must use https scheme")
	}
	if !strings.HasSuffix(sourceURL.Path, ".gguf") {
		return fmt.Errorf("source path must end with .gguf")
	}
	return nil
}

func evaluateRollout(deployment *appsv1.Deployment, desiredReplicas int32) rolloutState {
	if reason, message, failed := deploymentFailure(deployment); failed {
		return rolloutState{
			phase:   phaseFailed,
			status:  metav1.ConditionFalse,
			reason:  reason,
			message: message,
		}
	}
	if deploymentRolloutComplete(deployment, desiredReplicas) {
		return rolloutState{
			phase:   phaseReady,
			status:  metav1.ConditionTrue,
			reason:  "DeploymentReady",
			message: "Inference Deployment is ready",
		}
	}
	if deployment.Status.ObservedGeneration == 0 {
		return rolloutState{
			phase:   phasePending,
			status:  metav1.ConditionFalse,
			reason:  "DeploymentPending",
			message: "Waiting for Deployment status to be observed",
		}
	}
	return rolloutState{
		phase:   phaseDeploying,
		status:  metav1.ConditionFalse,
		reason:  "DeploymentProgressing",
		message: fmt.Sprintf("Waiting for %d replica(s) to become ready", desiredReplicas),
	}
}

func deploymentFailure(deployment *appsv1.Deployment) (string, string, bool) {
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse {
			return condition.Reason, condition.Message, true
		}
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return condition.Reason, condition.Message, true
		}
	}
	return "", "", false
}

func endpointFor(model *llmv1alpha1.Model) string {
	return fmt.Sprintf(
		"http://%s.%s.svc.cluster.local:%d",
		model.Name,
		model.Namespace,
		inferencePort,
	)
}

func labelsFor(model *llmv1alpha1.Model) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       inferenceAppName,
		"app.kubernetes.io/instance":   model.Name,
		"app.kubernetes.io/managed-by": "minillmkube",
	}
}

// deploymentRolloutComplete reports whether the observed Deployment status
// belongs to its current spec generation and every desired replica has been
// updated and become ready. A stale status still describes the previous
// generation, so the rollout is not complete even when replicas look ready.
func deploymentRolloutComplete(d *appsv1.Deployment, desired int32) bool {
	if d.Status.ObservedGeneration < d.Generation {
		return false
	}
	if d.Status.UpdatedReplicas < desired {
		return false
	}
	if d.Status.Replicas > d.Status.UpdatedReplicas {
		return false
	}
	if d.Status.AvailableReplicas < d.Status.UpdatedReplicas {
		return false
	}
	return true
}

func replicasFor(model *llmv1alpha1.Model) int32 {
	if model.Spec.Replicas == nil {
		return 1
	}
	return *model.Spec.Replicas
}

func imageFor(model *llmv1alpha1.Model) string {
	if model.Spec.Image == "" {
		return defaultImage
	}
	return model.Spec.Image
}

func contextSizeFor(model *llmv1alpha1.Model) int32 {
	if model.Spec.ContextSize == 0 {
		return defaultContext
	}
	return model.Spec.ContextSize
}

func serverArgs(model *llmv1alpha1.Model) []string {
	return []string{
		"--model", modelFilePathFor(model),
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(inferencePort),
		"--ctx-size", strconv.FormatInt(int64(contextSizeFor(model)), 10),
	}
}

// usesHostPathCache reports whether the operator was configured with a host
// directory shared by every Pod (MODEL_CACHE_HOST_PATH). Both the volume and
// the cache-prep init container depend on this single decision.
func usesHostPathCache() bool {
	return os.Getenv(modelCacheEnv) != ""
}

func modelVolume() corev1.Volume {
	if cachePath := os.Getenv(modelCacheEnv); cachePath != "" {
		hostPathType := corev1.HostPathDirectoryOrCreate
		return corev1.Volume{
			Name: modelVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: cachePath,
					Type: &hostPathType,
				},
			},
		}
	}
	return corev1.Volume{
		Name: modelVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}
}

func pointerTo[T any](value T) *T {
	return &value
}
