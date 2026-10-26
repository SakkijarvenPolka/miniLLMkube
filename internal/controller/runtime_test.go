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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	testHFSource   = "hf://Qwen/Qwen3-0.6B"
	testNonGGUFURL = "https://example.com/model.bin"
)

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name        string
		spec        llmv1alpha1.ModelSpec
		wantRuntime string
		wantFormat  string
		wantErr     bool
	}{
		{
			name:        "gguf url with no runtime",
			spec:        llmv1alpha1.ModelSpec{Source: testModelSource},
			wantRuntime: runtimeLlamaCpp,
			wantFormat:  formatGGUF,
		},
		{
			name:        "hf repository with no runtime",
			spec:        llmv1alpha1.ModelSpec{Source: testHFSource},
			wantRuntime: runtimeVLLM,
			wantFormat:  formatSafetensors,
		},
		{
			name:        "explicit gguf format wins over an hf source",
			spec:        llmv1alpha1.ModelSpec{Source: testHFSource, Format: formatGGUF},
			wantRuntime: runtimeLlamaCpp,
			wantFormat:  formatGGUF,
		},
		{
			name:        "explicit safetensors format",
			spec:        llmv1alpha1.ModelSpec{Source: testNonGGUFURL, Format: formatSafetensors},
			wantRuntime: runtimeVLLM,
			wantFormat:  formatSafetensors,
		},
		{
			name:        "explicit runtime wins over the source",
			spec:        llmv1alpha1.ModelSpec{Source: testHFSource, Runtime: runtimeLlamaCpp},
			wantRuntime: runtimeLlamaCpp,
			wantFormat:  formatGGUF,
		},
		{
			name:        "explicit vllm runtime",
			spec:        llmv1alpha1.ModelSpec{Source: testModelSource, Runtime: runtimeVLLM, Format: formatSafetensors},
			wantRuntime: runtimeVLLM,
			wantFormat:  formatSafetensors,
		},
		{
			name:    "vllm cannot load gguf",
			spec:    llmv1alpha1.ModelSpec{Source: testModelSource, Runtime: runtimeVLLM, Format: formatGGUF},
			wantErr: true,
		},
		{
			name:    "llamacpp cannot load safetensors",
			spec:    llmv1alpha1.ModelSpec{Source: testHFSource, Runtime: runtimeLlamaCpp, Format: formatSafetensors},
			wantErr: true,
		},
		{
			name:    "unknown source cannot be inferred",
			spec:    llmv1alpha1.ModelSpec{Source: testNonGGUFURL},
			wantErr: true,
		},
		{
			name:    "unknown runtime",
			spec:    llmv1alpha1.ModelSpec{Source: testModelSource, Runtime: "tensorrt"},
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend, format, err := resolveBackend(testModel(test.spec))
			if test.wantErr {
				if err == nil {
					t.Fatalf("resolveBackend() = %s/%s, want an error", backend.Name(), format)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveBackend() error = %v", err)
			}
			if backend.Name() != test.wantRuntime || format != test.wantFormat {
				t.Fatalf("resolveBackend() = %s/%s, want %s/%s",
					backend.Name(), format, test.wantRuntime, test.wantFormat)
			}
		})
	}
}

func TestValidateSource(t *testing.T) {
	tests := []struct {
		name    string
		source  string
		format  string
		wantErr bool
	}{
		{name: "gguf url", source: testModelSource, format: formatGGUF},
		{name: "gguf url with query", source: testModelSource + "?download=true", format: formatGGUF},
		{name: "gguf with http", source: "http://example.com/model.gguf", format: formatGGUF, wantErr: true},
		{name: "gguf without suffix", source: testNonGGUFURL, format: formatGGUF, wantErr: true},
		{name: "hf repository", source: testHFSource, format: formatSafetensors},
		{name: "hf repository with revision", source: testHFSource + "@main", format: formatSafetensors},
		{name: "hf repository with slashed revision", source: testHFSource + "@refs/pr/1", format: formatSafetensors},
		{name: "hf one segment", source: "hf://Qwen", format: formatSafetensors, wantErr: true},
		{name: "hf traversal", source: "hf://Qwen/..", format: formatSafetensors, wantErr: true},
		{name: "hf empty revision", source: testHFSource + "@", format: formatSafetensors, wantErr: true},
		{name: "hf whitespace revision", source: testHFSource + "@a b", format: formatSafetensors, wantErr: true},
		{name: "safetensors on an https url", source: testModelSource, format: formatSafetensors, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := testModel(llmv1alpha1.ModelSpec{Source: test.source})
			err := validateSource(model, test.format)
			if test.wantErr && err == nil {
				t.Fatalf("validateSource(%q) = nil, want an error", test.source)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateSource(%q) error = %v", test.source, err)
			}
		})
	}
}

func TestLlamacppArgs(t *testing.T) {
	model := testModel(llmv1alpha1.ModelSpec{})
	args := llamacppBackend{}.Args(model, "/models/key/model.gguf")

	if got := argValue(args, "--host"); got != "::" {
		t.Fatalf("--host = %q, want ::", got)
	}
	if got := argValue(args, "--alias"); got != model.Name {
		t.Fatalf("--alias = %q, want %q", got, model.Name)
	}
	if got := argValue(args, "--model"); got != "/models/key/model.gguf" {
		t.Fatalf("--model = %q", got)
	}
}

func TestVllmArgs(t *testing.T) {
	model := testModel(llmv1alpha1.ModelSpec{Source: testHFSource, ContextSize: 2048})
	args := vllmBackend{}.Args(model, "/models/key")

	for flag, want := range map[string]string{
		"--model":                  "/models/key",
		"--served-model-name":      model.Name,
		"--host":                   "::",
		"--port":                   "8080",
		"--max-model-len":          "2048",
		"--gpu-memory-utilization": vllmGPUMemoryUtilization,
	} {
		if got := argValue(args, flag); got != want {
			t.Fatalf("%s = %q, want %q: %v", flag, got, want, args)
		}
		if count := flagCount(args, flag); count != 1 {
			t.Fatalf("%s appears %d times: %v", flag, count, args)
		}
	}

	overriddenModel := testModel(llmv1alpha1.ModelSpec{
		Source:    testHFSource,
		ExtraArgs: []string{"--max-model-len", "4096"},
	})
	overridden := vllmBackend{}.Args(overriddenModel, "/models/key")
	if count := flagCount(overridden, "--max-model-len"); count != 1 {
		t.Fatalf("--max-model-len appears %d times after the override: %v", count, overridden)
	}
	if got := argValue(overridden, "--max-model-len"); got != "4096" {
		t.Fatalf("--max-model-len = %q, want the user value", got)
	}
}

func TestVllmPodMutations(t *testing.T) {
	memoryLimit := resource.MustParse("8Gi")
	model := testModel(llmv1alpha1.ModelSpec{
		Source:    testHFSource,
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: memoryLimit}},
	})
	spec := buildPodSpec(model)

	if spec.EnableServiceLinks == nil || *spec.EnableServiceLinks {
		t.Fatalf("EnableServiceLinks = %v, want false", spec.EnableServiceLinks)
	}

	var shm *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == sharedMemoryVolumeName {
			shm = &spec.Volumes[i]
		}
	}
	if shm == nil {
		t.Fatalf("missing %s volume: %+v", sharedMemoryVolumeName, spec.Volumes)
	}
	if shm.EmptyDir == nil || shm.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("shm volume is not a memory emptyDir: %+v", shm.EmptyDir)
	}
	if got, want := shm.EmptyDir.SizeLimit.Value(), memoryLimit.Value()/2; got != want {
		t.Fatalf("shm size = %d, want %d", got, want)
	}

	mounted := false
	for _, mount := range spec.Containers[0].VolumeMounts {
		if mount.Name == sharedMemoryVolumeName && mount.MountPath == sharedMemoryMountPath {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("inference container does not mount %s: %+v", sharedMemoryMountPath, spec.Containers[0].VolumeMounts)
	}
}

func TestSharedMemorySizeWithoutLimit(t *testing.T) {
	size := sharedMemorySize(testModel(llmv1alpha1.ModelSpec{Source: testHFSource}))
	if got := size.String(); got != defaultSharedMemorySize {
		t.Fatalf("sharedMemorySize() = %q, want %q", got, defaultSharedMemorySize)
	}
}

func TestVllmEnv(t *testing.T) {
	model := testModel(llmv1alpha1.ModelSpec{Source: testHFSource})

	t.Setenv(wsl2PinMemoryEnv, "")
	if env := (vllmBackend{}).Env(model); len(env) != 0 {
		t.Fatalf("Env() = %+v, want no variables by default", env)
	}

	t.Setenv(wsl2PinMemoryEnv, "true")
	env := (vllmBackend{}).Env(model)
	if len(env) != 1 || env[0].Name != vllmPinMemoryEnvVar || env[0].Value != "1" {
		t.Fatalf("Env() = %+v, want %s=1", env, vllmPinMemoryEnvVar)
	}
}

func TestVllmDownloader(t *testing.T) {
	t.Setenv(hfEndpointEnv, "https://hf-mirror.com")
	model := testModel(llmv1alpha1.ModelSpec{Source: testHFSource + "@v1.0"})

	container := vllmBackend{}.Downloader(model)
	if container.Name != downloaderContainerName {
		t.Fatalf("downloader name = %q", container.Name)
	}
	if container.Image != vllmImage {
		t.Fatalf("downloader image = %q, want %q", container.Image, vllmImage)
	}
	for name, want := range map[string]string{
		"MODEL_REPO":     "Qwen/Qwen3-0.6B",
		"MODEL_REVISION": "v1.0",
		"MODEL_DIR":      "/models/" + cacheKeyFor(testHFSource+"@v1.0"),
		hfEndpointEnv:    "https://hf-mirror.com",
	} {
		if got := envValue(container.Env, name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	if len(container.Args) != 1 || container.Args[0] != downloadScriptSafetensors {
		t.Fatalf("downloader args are not the safetensors script: %+v", container.Args)
	}
}
