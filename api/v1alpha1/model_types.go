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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ModelSpec defines the desired state of Model
//
// The runtime and format combination is restricted before the object is stored:
// vLLM cannot load GGUF weights, and llama.cpp cannot load safetensors weights.
// Both fields default to "auto" and are validated after defaulting.
//
// +kubebuilder:validation:XValidation:rule="!(self.runtime == 'vllm' && self.format == 'gguf')",message="vllm does not support gguf in this operator"
// +kubebuilder:validation:XValidation:rule="!(self.runtime == 'llamacpp' && self.format == 'safetensors')",message="llamacpp requires gguf"
type ModelSpec struct {
	// Source is the HTTPS download URL for GGUF model weights.
	// The controller validates that the parsed URL uses HTTPS and that the
	// path ends with ".gguf"; query strings such as "?download=true" are allowed.
	// Quantization is determined by the GGUF file referenced in the URL.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Source string `json:"source"`
	// Replicas is the desired number of inference Pods.
	// Set to 0 to scale the Deployment down to zero replicas.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// ContextSize is the model context window size in tokens.
	// +kubebuilder:default=4096
	// +kubebuilder:validation:Minimum=128
	// +optional
	ContextSize int32 `json:"contextSize,omitempty"`
	// Runtime selects the inference engine: "llamacpp" (GGUF weights) or "vllm"
	// (safetensors weights from the Hugging Face Hub). "auto" leaves the choice
	// to the operator, which infers it from Format and Source; the decision is
	// reported in status.resolvedRuntime and never written back to the spec.
	// +kubebuilder:validation:Enum=auto;llamacpp;vllm
	// +kubebuilder:default=auto
	// +optional
	Runtime string `json:"runtime,omitempty"`
	// Format is the weight format: "gguf" for llama.cpp, "safetensors" for vLLM.
	// "auto" infers it from Runtime, or from Source when Runtime is "auto".
	// +kubebuilder:validation:Enum=auto;gguf;safetensors
	// +kubebuilder:default=auto
	// +optional
	Format string `json:"format,omitempty"`
	// Image is the container image of the inference server.
	// When empty, the operator picks the engine image from the backend
	// (llama.cpp today) and from whether the Model asks for a GPU.
	// +optional
	Image string `json:"image,omitempty"`
	// Resources sets CPU and memory requests and limits for the inference container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// GPU requests GPU acceleration for the inference server.
	// Setting it at all (even to "{}") is what enables GPU mode: the operator
	// then uses the CUDA image, requests nvidia.com/gpu and offloads Layers
	// layers of the model to the GPU. Leaving it nil runs the CPU engine.
	// +optional
	GPU *GPUSpec `json:"gpu,omitempty"`
	// RuntimeClassName is the RuntimeClass of the inference Pod, for example
	// "nvidia". When empty and GPU mode is enabled, the operator falls back to
	// the GPU_RUNTIME_CLASS environment variable of its own Deployment, and then
	// to no RuntimeClass at all (the cluster default).
	// +optional
	RuntimeClassName *string `json:"runtimeClassName,omitempty"`
	// ExtraArgs are appended verbatim to the engine command line, after the
	// arguments the operator generates. Use it for anything the spec does not
	// expose; arguments the operator already set are not duplicated.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
	// NodeSelector is passed to the inference Pod verbatim, so a Model can be
	// pinned to a node label without the operator knowing about the node pool.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Tolerations are passed to the inference Pod verbatim, for example to run on
	// a node tainted for GPU workloads.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// Affinity is passed to the inference Pod verbatim.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// SchedulerName is the scheduler that places the inference Pod, for example a
	// second scheduler or a custom plugin. When empty the operator writes
	// "default-scheduler" explicitly, so the stored PodSpec never differs from the
	// rendered one.
	// +optional
	SchedulerName string `json:"schedulerName,omitempty"`
}

// GPUSpec describes the GPU resources for one Model.
type GPUSpec struct {
	// Count is the number of GPUs requested as nvidia.com/gpu.
	// Only 1 is supported today; a larger value is rejected by the CRD.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1
	// +optional
	Count int32 `json:"count,omitempty"`
	// Layers is the number of model layers offloaded to the GPU,
	// passed to llama.cpp as --n-gpu-layers. 99 offloads every layer of the
	// small models this operator targets.
	// +kubebuilder:default=99
	// +kubebuilder:validation:Minimum=0
	// +optional
	Layers int32 `json:"layers,omitempty"`
}

// ModelStatus defines the observed state of Model
type ModelStatus struct {
	// Phase 人类可读的状态概览: Pending / Deploying / Ready / Failed
	// +optional
	Phase string `json:"phase,omitempty"`
	// Endpoint 集群内 OpenAI 兼容接口地址
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
	// ReadyReplicas 就绪副本数
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// CacheKey 缓存目录名，等于 sha256(spec.source) 的前 12 个十六进制字符。
	// 模型文件位于缓存根目录下 <CacheKey>/ 子目录，便于到宿主机上定位文件。
	// +optional
	CacheKey string `json:"cacheKey,omitempty"`
	// ResolvedRuntime 实际使用的推理后端: llamacpp / vllm
	// +optional
	ResolvedRuntime string `json:"resolvedRuntime,omitempty"`
	// ResolvedFormat 实际使用的权重格式: gguf / safetensors
	// +optional
	ResolvedFormat string `json:"resolvedFormat,omitempty"`
	// ObservedGeneration 已处理的 spec 版本，用于判断 status 是否过期
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mdl
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.status.resolvedRuntime`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Model struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Model
	// +required
	Spec ModelSpec `json:"spec"`

	// status defines the observed state of Model
	// +optional
	Status ModelStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Model
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Model{}, &ModelList{})
}
