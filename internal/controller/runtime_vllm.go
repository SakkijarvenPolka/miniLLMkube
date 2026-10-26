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
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	// vllmImage is the vLLM OpenAI-compatible server. The tag is pinned and
	// built on CUDA 12.9, which the operator's target driver supports; nightly
	// builds need a newer driver (F13).
	vllmImage = "vllm/vllm-openai:v0.32.0-cu129"

	// vllmStartupFailureThreshold is 240 probes x 5s = 20 minutes. vLLM loads
	// the weights and captures CUDA graphs on startup, which takes minutes even
	// for a small model. M3-T0 replaces this estimate with a measurement.
	vllmStartupFailureThreshold = int32(240)

	// vllmGPUMemoryUtilization leaves headroom for the desktop that shares the
	// GPU: 12GB minus ~1.2GB used by the WSL desktop (E23).
	vllmGPUMemoryUtilization = "0.85"

	// wsl2PinMemoryEnv is set on the operator Deployment; when it is "true" the
	// operator injects VLLM_WSL2_ENABLE_PIN_MEMORY=1 into the vLLM container.
	// Only WSL2 needs it (E22).
	wsl2PinMemoryEnv    = "ENABLE_WSL2_VLLM_PIN_MEMORY"
	vllmPinMemoryEnvVar = "VLLM_WSL2_ENABLE_PIN_MEMORY"

	// hfEndpointEnv is the operator environment variable forwarded to the
	// downloader so that it can use a mirror.
	hfEndpointEnv = "HF_ENDPOINT"

	// sharedMemoryVolumeName backs /dev/shm of the vLLM container.
	sharedMemoryVolumeName = "shm"
	// sharedMemoryMountPath is where vLLM puts its multiprocessing buffers. The
	// container runtime default is only 64MiB, which breaks vLLM at startup
	// (E14).
	sharedMemoryMountPath = "/dev/shm"
	// defaultSharedMemorySize is used when the Model sets no memory limit.
	defaultSharedMemorySize = "1Gi"
)

// vllmBackend runs vLLM's OpenAI-compatible server on a safetensors directory.
type vllmBackend struct{}

func (vllmBackend) Name() string { return runtimeVLLM }

// DefaultImage ignores the GPU flag: vLLM needs CUDA regardless of how the
// Model asks for it.
func (vllmBackend) DefaultImage(bool) string { return vllmImage }

// Args renders the vLLM command line. Arguments the operator generates are
// skipped when the user already set them through spec.extraArgs (E16), and the
// user's arguments always come last.
func (vllmBackend) Args(model *llmv1alpha1.Model, modelPath string) []string {
	generated := [][2]string{
		{"--model", modelPath},
		{"--served-model-name", model.Name},
		{"--host", inferenceBindAddress},
		{"--port", strconv.Itoa(inferencePort)},
		{"--max-model-len", strconv.FormatInt(int64(contextSizeFor(model)), 10)},
		{"--gpu-memory-utilization", vllmGPUMemoryUtilization},
	}

	args := make([]string, 0, len(generated)*2+len(model.Spec.ExtraArgs))
	for _, pair := range generated {
		if hasFlag(model.Spec.ExtraArgs, pair[0]) {
			continue
		}
		args = append(args, pair[0], pair[1])
	}
	return append(args, model.Spec.ExtraArgs...)
}

// Env keeps the operator's environment out of the container unless it was asked
// for: vLLM validates unknown VLLM_* variables and refuses to start.
func (vllmBackend) Env(*llmv1alpha1.Model) []corev1.EnvVar {
	var env []corev1.EnvVar
	if strings.EqualFold(os.Getenv(wsl2PinMemoryEnv), "true") {
		env = append(env, corev1.EnvVar{Name: vllmPinMemoryEnvVar, Value: "1"})
	}
	return env
}

func (vllmBackend) HealthPath() string { return healthPath }

func (vllmBackend) StartupFailureThreshold() int32 { return vllmStartupFailureThreshold }

// ModelPath is the cache directory holding config.json and the weights.
func (vllmBackend) ModelPath(model *llmv1alpha1.Model) string {
	return modelDirFor(model)
}

func (vllmBackend) Downloader(model *llmv1alpha1.Model) corev1.Container {
	source, err := parseHFSource(model.Spec.Source)
	if err != nil {
		// validateSpec rejected this Model before the Pod template was
		// rendered; keep the downloader well-formed for defensive callers.
		source = hfSource{repo: model.Spec.Source}
	}

	env := []corev1.EnvVar{
		{Name: "MODEL_REPO", Value: source.repo},
		{Name: "MODEL_REVISION", Value: source.revision},
		{Name: "MODEL_DIR", Value: modelDirFor(model)},
	}
	if endpoint := os.Getenv(hfEndpointEnv); endpoint != "" {
		env = append(env, corev1.EnvVar{Name: hfEndpointEnv, Value: endpoint})
	}

	return corev1.Container{
		Name:                     downloaderContainerName,
		Image:                    vllmImage,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		Command:                  []string{"sh", "-c"},
		Args:                     []string{downloadScriptSafetensors},
		Env:                      env,
		VolumeMounts:             []corev1.VolumeMount{{Name: modelVolumeName, MountPath: modelMountPath}},
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

// PodMutations gives vLLM a /dev/shm large enough for its multiprocessing
// buffers and turns off service links: the injected variables confuse its
// configuration parser (E14, E15).
func (vllmBackend) PodMutations(spec *corev1.PodSpec, model *llmv1alpha1.Model) {
	spec.EnableServiceLinks = pointerTo(false)
	spec.Volumes = append(spec.Volumes, corev1.Volume{
		Name: sharedMemoryVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: sharedMemorySize(model),
		}},
	})
	for i := range spec.Containers {
		if spec.Containers[i].Name != inferenceContainerName {
			continue
		}
		spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{Name: sharedMemoryVolumeName, MountPath: sharedMemoryMountPath})
	}
}

// sharedMemorySize gives /dev/shm half of the memory limit, the value the vLLM
// documentation suggests for a container. Without a limit it falls back to 1Gi.
func sharedMemorySize(model *llmv1alpha1.Model) *resource.Quantity {
	limit, limited := model.Spec.Resources.Limits[corev1.ResourceMemory]
	if !limited || limit.IsZero() {
		size := resource.MustParse(defaultSharedMemorySize)
		return &size
	}
	size := *resource.NewQuantity(limit.Value()/2, resource.BinarySI)
	return &size
}
