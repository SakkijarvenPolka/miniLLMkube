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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	inferenceContainerName  = "llama-server"
	downloaderContainerName = "model-downloader"
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
		Volumes:        []corev1.Volume{modelVolume()},
		InitContainers: initContainersFor(model),
		Containers:     []corev1.Container{inferenceContainer(model)},
		SchedulerName:  "default-scheduler", // M5 will pass the user value through
	}
}

// initContainersFor returns the init containers in execution order.
func initContainersFor(model *llmv1alpha1.Model) []corev1.Container {
	return []corev1.Container{downloaderContainer(model)}
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
		Resources:       model.Spec.Resources,
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
			Path: "/health", Port: intstr.FromString("http"),
		}},
		PeriodSeconds: 5, TimeoutSeconds: 1, SuccessThreshold: 1, FailureThreshold: 120,
	}
}

// readinessProbe keeps the values the API server used to default to
// (10/3/1/3), so making them explicit does not roll existing Deployments.
func readinessProbe() *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health", Port: intstr.FromString("http"),
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
