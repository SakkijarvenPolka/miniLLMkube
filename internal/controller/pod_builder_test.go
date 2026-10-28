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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	customImage   = "example.com/custom:1"
	nvidiaRuntime = "nvidia"
	// Placement values shared by the unit and envtest cases.
	testNodeArch  = "kubernetes.io/arch"
	testArchAMD64 = "amd64"
	testScheduler = "custom-scheduler"
	testTrueValue = "true"
)

func testModel(spec llmv1alpha1.ModelSpec) *llmv1alpha1.Model {
	if spec.Source == "" {
		spec.Source = testModelSource
	}
	return &llmv1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: testUnitModelName, Namespace: testNamespace},
		Spec:       spec,
	}
}

// flagCount counts the arguments that name one of the flags, in both the
// "--flag value" and the "--flag=value" spelling.
func flagCount(args []string, names ...string) int {
	count := 0
	for _, arg := range args {
		for _, name := range names {
			if arg == name || strings.HasPrefix(arg, name+"=") {
				count++
			}
		}
	}
	return count
}

func TestInferenceContainerGPU(t *testing.T) {
	gpuOneLayer99 := &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}
	gpuRequest := corev1.ResourceList{
		corev1.ResourceName(gpuResourceName): resource.MustParse("1"),
	}

	tests := []struct {
		name string
		spec llmv1alpha1.ModelSpec
		// envRuntimeClass is exported as GPU_RUNTIME_CLASS for the case.
		envRuntimeClass string
		wantImage       string
		wantRuntime     *string
		wantGPURequests string // "" means nvidia.com/gpu must not be present
		wantLayers      int    // number of --n-gpu-layers occurrences
		wantLayersValue string // value expected next to --n-gpu-layers
		wantVerbosity   int    // number of -lv occurrences
	}{
		{
			name:      "cpu model",
			spec:      llmv1alpha1.ModelSpec{},
			wantImage: defaultImage,
		},
		{
			name:            "gpu block with crd defaults applied",
			spec:            llmv1alpha1.ModelSpec{GPU: gpuOneLayer99},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name:            "gpu block with an explicit layer count",
			spec:            llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 12}},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "12",
			wantVerbosity:   1,
		},
		{
			name:            "gpu block with zero layers is honoured",
			spec:            llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 0}},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "0",
			wantVerbosity:   1,
		},
		{
			name: "gpu requested through resource limits only",
			spec: llmv1alpha1.ModelSpec{
				Resources: corev1.ResourceRequirements{Limits: gpuRequest},
			},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name: "user already set -ngl",
			spec: llmv1alpha1.ModelSpec{
				GPU:       gpuOneLayer99,
				ExtraArgs: []string{"-ngl", "20"},
			},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "20",
			wantVerbosity:   1,
		},
		{
			name: "user already set --n-gpu-layers=20",
			spec: llmv1alpha1.ModelSpec{
				GPU:       gpuOneLayer99,
				ExtraArgs: []string{"--n-gpu-layers=20"},
			},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "20",
			wantVerbosity:   1,
		},
		{
			name: "user already set -lv",
			spec: llmv1alpha1.ModelSpec{
				GPU:       gpuOneLayer99,
				ExtraArgs: []string{"-lv", "4"},
			},
			wantImage:       defaultGPUImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name: "explicit image wins",
			spec: llmv1alpha1.ModelSpec{
				Image: customImage,
				GPU:   gpuOneLayer99,
			},
			wantImage:       customImage,
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name: "explicit runtime class wins",
			spec: llmv1alpha1.ModelSpec{
				GPU:              gpuOneLayer99,
				RuntimeClassName: pointerTo("cluster-gpu"),
			},
			envRuntimeClass: nvidiaRuntime,
			wantImage:       defaultGPUImage,
			wantRuntime:     pointerTo("cluster-gpu"),
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name:            "runtime class from the operator environment",
			spec:            llmv1alpha1.ModelSpec{GPU: gpuOneLayer99},
			envRuntimeClass: nvidiaRuntime,
			wantImage:       defaultGPUImage,
			wantRuntime:     pointerTo(nvidiaRuntime),
			wantGPURequests: "1",
			wantLayers:      1,
			wantLayersValue: "99",
			wantVerbosity:   1,
		},
		{
			name:            "cpu model ignores the runtime class environment",
			spec:            llmv1alpha1.ModelSpec{},
			envRuntimeClass: nvidiaRuntime,
			wantImage:       defaultImage,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(gpuRuntimeClassEnv, test.envRuntimeClass)

			model := testModel(test.spec)
			built := buildPodSpec(model)

			if built.RuntimeClassName != nil {
				if test.wantRuntime == nil {
					t.Fatalf("RuntimeClassName = %q, want nil", *built.RuntimeClassName)
				}
				if *built.RuntimeClassName != *test.wantRuntime {
					t.Fatalf("RuntimeClassName = %q, want %q", *built.RuntimeClassName, *test.wantRuntime)
				}
			} else if test.wantRuntime != nil {
				t.Fatalf("RuntimeClassName = nil, want %q", *test.wantRuntime)
			}

			container := built.Containers[0]
			if container.Image != test.wantImage {
				t.Fatalf("image = %q, want %q", container.Image, test.wantImage)
			}

			gpuLimit, hasLimit := container.Resources.Limits[corev1.ResourceName(gpuResourceName)]
			if test.wantGPURequests == "" {
				if hasLimit {
					t.Fatalf("unexpected %s limit %q", gpuResourceName, gpuLimit.String())
				}
			} else {
				if !hasLimit {
					t.Fatalf("missing %s limit", gpuResourceName)
				}
				if got := gpuLimit.String(); got != test.wantGPURequests {
					t.Fatalf("%s limit = %q, want %q", gpuResourceName, got, test.wantGPURequests)
				}
				// The API server mirrors limits into requests; the rendered
				// object has to match or every reconcile would Update (F9).
				gpuRequest := container.Resources.Requests[corev1.ResourceName(gpuResourceName)]
				if got, want := gpuRequest.String(), test.wantGPURequests; got != want {
					t.Fatalf("%s request = %q, want %q", gpuResourceName, got, want)
				}
			}

			if got := flagCount(container.Args, "--n-gpu-layers", "--gpu-layers", "-ngl"); got != test.wantLayers {
				t.Fatalf("--n-gpu-layers occurrences = %d, want %d: %v", got, test.wantLayers, container.Args)
			}
			if test.wantLayersValue != "" {
				if got := argValue(container.Args, "--n-gpu-layers", "--gpu-layers", "-ngl"); got != test.wantLayersValue {
					t.Fatalf("--n-gpu-layers value = %q, want %q: %v", got, test.wantLayersValue, container.Args)
				}
			}
			if got := flagCount(container.Args, "--verbosity", "-v", "-lv"); got != test.wantVerbosity {
				t.Fatalf("-lv occurrences = %d, want %d: %v", got, test.wantVerbosity, container.Args)
			}

			if extra := test.spec.ExtraArgs; len(extra) > 0 {
				found := false
				for i := 0; i+len(extra) <= len(container.Args); i++ {
					if strings.Join(container.Args[i:i+len(extra)], "\x00") == strings.Join(extra, "\x00") {
						found = true
						if i+len(extra) != len(container.Args) {
							t.Fatalf("extraArgs are not last: %v", container.Args)
						}
						break
					}
				}
				if !found {
					t.Fatalf("extraArgs %v missing from %v", extra, container.Args)
				}
			}
		})
	}
}

// argValue returns the value of the first of names found in args, in either the
// "--flag value" or the "--flag=value" spelling.
func argValue(args []string, names ...string) string {
	for i, arg := range args {
		for _, name := range names {
			if arg == name && i+1 < len(args) {
				return args[i+1]
			}
			if value, found := strings.CutPrefix(arg, name+"="); found {
				return value
			}
		}
	}
	return ""
}

func TestApplyPodSpecKeepsDefaultedFields(t *testing.T) {
	dst := corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyAlways,
		DNSPolicy:          corev1.DNSClusterFirst,
		SchedulerName:      "default-scheduler",
		EnableServiceLinks: pointerTo(true),
	}
	src := buildPodSpec(testModel(llmv1alpha1.ModelSpec{}))

	applyPodSpec(&dst, src)

	if dst.Containers == nil || len(dst.Containers) != 1 {
		t.Fatalf("containers were not applied: %+v", dst.Containers)
	}
	if dst.RestartPolicy != corev1.RestartPolicyAlways || dst.DNSPolicy != corev1.DNSClusterFirst {
		t.Fatalf("applyPodSpec touched defaulted fields: %+v", dst)
	}
	if dst.EnableServiceLinks == nil || !*dst.EnableServiceLinks {
		t.Fatalf("applyPodSpec overwrote EnableServiceLinks when the source left it nil")
	}
	if dst.SchedulerName != "default-scheduler" {
		t.Fatalf("SchedulerName = %q", dst.SchedulerName)
	}
}

func TestImageForPrefersSpec(t *testing.T) {
	model := testModel(llmv1alpha1.ModelSpec{Image: customImage, GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}})
	if got := imageFor(model); got != customImage {
		t.Fatalf("imageFor() = %q", got)
	}
}

func TestHasFlag(t *testing.T) {
	args := []string{"--ctx-size", "4096", "--n-gpu-layers=12", "-lv", "5"}
	if !hasFlag(args, "--n-gpu-layers") {
		t.Fatalf("hasFlag missed --n-gpu-layers=12")
	}
	if !hasFlag(args, "-lv") {
		t.Fatalf("hasFlag missed -lv")
	}
	if hasFlag(args, "--gpu-layers") {
		t.Fatalf("hasFlag matched --gpu-layers inside --n-gpu-layers")
	}
	if hasFlag(nil, "--model") {
		t.Fatalf("hasFlag matched an empty argument list")
	}
}
func TestBuildPodSpecPlacement(t *testing.T) {
	t.Run("defaults the scheduler and leaves placement empty", func(t *testing.T) {
		spec := buildPodSpec(testModel(llmv1alpha1.ModelSpec{}))

		if spec.SchedulerName != defaultSchedulerName {
			t.Errorf("schedulerName = %q, want %q", spec.SchedulerName, defaultSchedulerName)
		}
		if spec.NodeSelector != nil {
			t.Errorf("nodeSelector = %v, want nil", spec.NodeSelector)
		}
		if spec.Tolerations != nil {
			t.Errorf("tolerations = %v, want nil", spec.Tolerations)
		}
		if spec.Affinity != nil {
			t.Errorf("affinity = %v, want nil", spec.Affinity)
		}
	})

	t.Run("passes every placement field through", func(t *testing.T) {
		model := testModel(llmv1alpha1.ModelSpec{
			NodeSelector: map[string]string{testNodeArch: testTrueValue},
			Tolerations: []corev1.Toleration{{
				Key:      gpuResourceName,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      testNodeArch,
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{testArchAMD64},
							}},
						}},
					},
				},
			},
			SchedulerName: testScheduler,
		})

		spec := buildPodSpec(model)

		if spec.SchedulerName != testScheduler {
			t.Errorf("schedulerName = %q, want custom-scheduler", spec.SchedulerName)
		}
		if got := spec.NodeSelector[testNodeArch]; got != testTrueValue {
			t.Errorf("nodeSelector = %v, want the Model's map", spec.NodeSelector)
		}
		if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != gpuResourceName {
			t.Errorf("tolerations = %v, want the Model's toleration", spec.Tolerations)
		}
		if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil {
			t.Fatalf("affinity = %v, want the Model's affinity", spec.Affinity)
		}

		t.Log("keeping placement out of the selector labels")

		for key := range spec.NodeSelector {
			if _, inLabels := labelsFor(model)[key]; inLabels {
				t.Errorf("placement key %q leaked into the selector labels", key)
			}
		}
	})
}
