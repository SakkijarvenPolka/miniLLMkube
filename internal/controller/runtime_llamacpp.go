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
	"strconv"

	corev1 "k8s.io/api/core/v1"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	// llamacppStartupFailureThreshold is 120 probes x 5s = 10 minutes, enough
	// for loading the model and creating the CUDA context.
	llamacppStartupFailureThreshold = int32(120)
)

// llamacppBackend runs llama.cpp's server on a GGUF file.
type llamacppBackend struct{}

func (llamacppBackend) Name() string { return runtimeLlamaCpp }

func (llamacppBackend) DefaultImage(gpu bool) string {
	if gpu {
		return defaultGPUImage
	}
	return defaultImage
}

// Args renders the llama-server command line. Arguments the operator generates
// are skipped when the user already set them through spec.extraArgs, and the
// user's arguments always come last so they win on conflicts (E16).
func (llamacppBackend) Args(model *llmv1alpha1.Model, modelPath string) []string {
	args := []string{
		"--model", modelPath,
		"--host", inferenceBindAddress,
		"--port", strconv.Itoa(inferencePort),
		"--ctx-size", strconv.FormatInt(int64(contextSizeFor(model)), 10),
		// --alias makes /v1/models report the Model name instead of the path on
		// the node (E17).
		"--alias", model.Name,
	}
	if wantsGPU(model) {
		if !hasFlag(model.Spec.ExtraArgs, "--n-gpu-layers", "--gpu-layers", "-ngl") {
			args = append(args, "--n-gpu-layers", strconv.Itoa(int(gpuLayersFor(model))))
		}
		// -lv 5 raises the log level so llama.cpp prints the device it loads
		// the model onto; the GPUOffload condition is parsed from that line.
		if !hasFlag(model.Spec.ExtraArgs, "--verbosity", "-v", "-lv") {
			args = append(args, "-lv", gpuVerbosityLevel)
		}
	}
	return append(args, model.Spec.ExtraArgs...)
}

func (llamacppBackend) Env(*llmv1alpha1.Model) []corev1.EnvVar { return nil }

func (llamacppBackend) HealthPath() string { return healthPath }

func (llamacppBackend) StartupFailureThreshold() int32 { return llamacppStartupFailureThreshold }

// ModelPath is the downloaded GGUF file.
func (llamacppBackend) ModelPath(model *llmv1alpha1.Model) string {
	return modelFilePathFor(model)
}

func (llamacppBackend) Downloader(model *llmv1alpha1.Model) corev1.Container {
	return ggufDownloaderContainer(model)
}

// PodMutations is empty: llama.cpp needs no Pod-level changes.
func (llamacppBackend) PodMutations(*corev1.PodSpec, *llmv1alpha1.Model) {}
