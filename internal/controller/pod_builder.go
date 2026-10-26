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
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	inferenceContainerName  = "llama-server"
	downloaderContainerName = "model-downloader"
	cachePrepContainerName  = "cache-prep"

	defaultImage    = "ghcr.io/ggml-org/llama.cpp:server"
	defaultGPUImage = "ghcr.io/ggml-org/llama.cpp:server-cuda"

	// gpuResourceName is the extended resource the device plugin advertises.
	gpuResourceName = "nvidia.com/gpu"
	// gpuRuntimeClassEnv is consulted when a GPU Model does not name a
	// RuntimeClass itself; it holds the cluster's GPU RuntimeClass.
	gpuRuntimeClassEnv = "GPU_RUNTIME_CLASS"
	// gpuVerbosityLevel is passed as -lv so llama.cpp logs the device it loads
	// the model onto.
	gpuVerbosityLevel = "5"
	// defaultGPULayers is the offload count used when the Model has no gpu
	// block; it matches the CRD default.
	defaultGPULayers = 99
)

// buildPodSpec renders the Pod template of the inference Deployment. It is a
// pure function of the Model and of the operator's environment, so it can be
// unit tested without an API server.
//
// Every field the API server would default on its own is written explicitly
// here. controller-runtime's CreateOrUpdate compares the mutated object with
// the stored one and issues an Update on any difference, so leaving a
// defaulted field unset would produce one useless Update per reconcile.
func buildPodSpec(model *llmv1alpha1.Model) corev1.PodSpec {
	return corev1.PodSpec{
		Volumes:          []corev1.Volume{modelVolume()},
		InitContainers:   initContainersFor(model),
		Containers:       []corev1.Container{inferenceContainer(model)},
		RuntimeClassName: runtimeClassFor(model),
		SchedulerName:    "default-scheduler", // M5 will pass the user value through
	}
}

// initContainersFor returns the init containers in execution order: prepare
// the cache root first, then download the model file.
func initContainersFor(model *llmv1alpha1.Model) []corev1.Container {
	var containers []corev1.Container
	if usesHostPathCache() {
		containers = append(containers, cachePrepContainer())
	}
	return append(containers, downloaderContainer(model))
}

// cachePrepContainer hands the mounted cache root to the downloader user. It
// only runs with a hostPath cache: the host directory is created by the kubelet
// as root while the downloader runs as uid 100.
//
// chown is the entry process itself rather than "sh -c chown": a non-root
// process loses its added capabilities across execve (E10). Only the mount root
// is chowned, not recursively, and subdirectories are created by the downloader
// itself — root only holds CHOWN here, not DAC_OVERRIDE.
func cachePrepContainer() corev1.Container {
	return corev1.Container{
		Name:         cachePrepContainerName,
		Image:        downloaderImage,
		Command:      []string{"chown", "100:101", modelMountPath},
		VolumeMounts: []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                pointerTo(int64(0)),
			RunAsNonRoot:             pointerTo(false),
			AllowPrivilegeEscalation: pointerTo(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"CHOWN"},
			},
		},
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
}

// downloaderContainer downloads the model file into the per-source cache
// directory. It runs as uid 100 (the image's user) and therefore only touches
// the model volume.
func downloaderContainer(model *llmv1alpha1.Model) corev1.Container {
	return corev1.Container{
		Name:            downloaderContainerName,
		Image:           downloaderImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"sh", "-c"},
		Args:            []string{downloadScriptGGUF},
		Env: []corev1.EnvVar{
			{Name: "MODEL_URL", Value: model.Spec.Source},
			{Name: "MODEL_DIR", Value: modelDirFor(model)},
			{Name: "MODEL_FILE", Value: modelFileName(model.Spec.Source)},
			{Name: "EXPECT_FORMAT", Value: "gguf"}, // M3 will let the backend decide
		},
		VolumeMounts:             []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

// inferenceContainer runs the inference server.
func inferenceContainer(model *llmv1alpha1.Model) corev1.Container {
	return corev1.Container{
		Name:            inferenceContainerName,
		Image:           imageFor(model),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            serverArgs(model),
		Resources:       resourcesFor(model),
		Ports: []corev1.ContainerPort{{
			Name: "http", ContainerPort: inferencePort, Protocol: corev1.ProtocolTCP,
		}},
		VolumeMounts:             []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		StartupProbe:             startupProbe(),
		ReadinessProbe:           readinessProbe(),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
}

// startupProbe waits for the engine to finish loading the model and its CUDA
// context: 5s x 120 = 10 minutes.
func startupProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			// Scheme is explicit for the same reason as the counters below: the
			// API server defaults it to HTTP, and an unset field would make the
			// stored template differ from the rendered one on every reconcile.
			Path: "/health", Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP,
		}},
		PeriodSeconds: 5, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 120,
	}
}

// readinessProbe keeps the values the API server used to default to
// (10/3/1/3), so making them explicit does not roll existing Deployments.
func readinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			// Scheme is explicit for the same reason as the counters below: the
			// API server defaults it to HTTP, and an unset field would make the
			// stored template differ from the rendered one on every reconcile.
			Path: "/health", Port: intstr.FromString("http"), Scheme: corev1.URISchemeHTTP,
		}},
		PeriodSeconds: 10, TimeoutSeconds: 3, SuccessThreshold: 1, FailureThreshold: 3,
	}
}

// applyPodSpec copies only the fields this operator owns into dst. Replacing
// the whole PodSpec would drop the values the API server defaulted into the
// stored template (restartPolicy, dnsPolicy, ...) and bring back one Update
// per reconcile.
func applyPodSpec(dst *corev1.PodSpec, src corev1.PodSpec) {
	dst.Volumes = src.Volumes
	dst.InitContainers = src.InitContainers
	dst.Containers = src.Containers
	dst.RuntimeClassName = src.RuntimeClassName // never defaulted by the API server; nil is a valid explicit value
	dst.NodeSelector = src.NodeSelector         // M5 will pass these through from the spec
	dst.Tolerations = src.Tolerations           // M5 will pass these through from the spec
	dst.Affinity = src.Affinity                 // M5 will pass these through from the spec
	dst.SchedulerName = src.SchedulerName       // the builder always writes an explicit value
	if src.EnableServiceLinks != nil {          // used by the vLLM backend in M3; nil keeps the stored value
		dst.EnableServiceLinks = src.EnableServiceLinks
	}
}

// wantsGPU reports whether the Model asks for GPU acceleration: either through
// spec.gpu or by requesting nvidia.com/gpu in its own resource limits.
func wantsGPU(model *llmv1alpha1.Model) bool {
	if model.Spec.GPU != nil {
		return true
	}
	_, requested := model.Spec.Resources.Limits[corev1.ResourceName(gpuResourceName)]
	return requested
}

// resourcesFor renders the inference container resources. spec.gpu implies an
// nvidia.com/gpu limit unless the user wrote one, and every limit is mirrored
// into requests the way the API server would: without that, the stored pod
// template would differ from the rendered one on every reconcile (F9).
func resourcesFor(model *llmv1alpha1.Model) corev1.ResourceRequirements {
	resources := *model.Spec.Resources.DeepCopy()

	if model.Spec.GPU != nil {
		if _, set := resources.Limits[corev1.ResourceName(gpuResourceName)]; !set {
			// The CRD default fills count on API-created objects; older objects
			// or typed clients can still hand over a zero value.
			count := max(model.Spec.GPU.Count, 1)
			if resources.Limits == nil {
				resources.Limits = corev1.ResourceList{}
			}
			resources.Limits[corev1.ResourceName(gpuResourceName)] = *resource.NewQuantity(int64(count), resource.DecimalSI)
		}
	}

	if len(resources.Limits) > 0 {
		if resources.Requests == nil {
			resources.Requests = corev1.ResourceList{}
		}
		for name, quantity := range resources.Limits {
			if _, set := resources.Requests[name]; !set {
				resources.Requests[name] = quantity
			}
		}
	}

	return resources
}

// gpuLayersFor returns how many model layers to offload. The CRD default of 99
// covers the normal "gpu: {}" case; a Model that only requests nvidia.com/gpu
// through its resource limits gets the same default because there is no gpu
// block to read it from. An explicit 0 is honoured: it means "load the model on
// the GPU engine but offload nothing".
func gpuLayersFor(model *llmv1alpha1.Model) int32 {
	if model.Spec.GPU == nil {
		return defaultGPULayers
	}
	return model.Spec.GPU.Layers
}

// imageFor picks the engine image. An explicit spec.image always wins; otherwise
// the GPU engine is used exactly when the Model asks for a GPU.
func imageFor(model *llmv1alpha1.Model) string {
	if model.Spec.Image != "" {
		return model.Spec.Image
	}
	if wantsGPU(model) {
		return defaultGPUImage
	}
	return defaultImage
}

// runtimeClassFor picks the RuntimeClass of the inference Pod. The spec wins,
// then GPU_RUNTIME_CLASS for GPU Models; a CPU Model never gets one from the
// environment.
func runtimeClassFor(model *llmv1alpha1.Model) *string {
	if model.Spec.RuntimeClassName != nil {
		return model.Spec.RuntimeClassName
	}
	if !wantsGPU(model) {
		return nil
	}
	if name := os.Getenv(gpuRuntimeClassEnv); name != "" {
		return pointerTo(name)
	}
	return nil
}

// hasFlag reports whether args already contain one of the flags, in either the
// "--flag value" or the "--flag=value" form.
func hasFlag(args []string, names ...string) bool {
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				return true
			}
		}
	}
	return false
}
