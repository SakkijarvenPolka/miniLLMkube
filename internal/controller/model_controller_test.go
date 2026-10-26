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
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

var _ = Describe("Model Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName = "test-resource"
			modelSource  = "https://huggingface.co/bartowski/Qwen_Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q4_K_M.gguf"
			modelImage   = "ghcr.io/ggml-org/llama.cpp:server"
		)

		ctx := context.Background()
		replicas := int32(1)

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: testNamespace,
		}
		model := &llmv1alpha1.Model{}
		var controllerReconciler *ModelReconciler

		BeforeEach(func() {
			controllerReconciler = &ModelReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}

			By("creating the custom resource for the Kind Model")
			err := k8sClient.Get(ctx, typeNamespacedName, model)
			if err != nil && errors.IsNotFound(err) {
				resource := &llmv1alpha1.Model{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: testNamespace,
					},
					Spec: llmv1alpha1.ModelSpec{
						Source:      modelSource,
						Replicas:    &replicas,
						ContextSize: 4096,
						Image:       modelImage,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			Expect(k8sClient.DeleteAllOf(ctx, &appsv1.Deployment{},
				client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &corev1.Service{},
				client.InNamespace(typeNamespacedName.Namespace))).To(Succeed())

			resource := &llmv1alpha1.Model{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance Model")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("creates the inference Deployment and Service", func() {
			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.Replicas).To(HaveValue(Equal(int32(1))))
			Expect(deployment.Spec.ProgressDeadlineSeconds).To(HaveValue(Equal(int32(1800))))
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
			Expect(deployment.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Volumes[0].EmptyDir).NotTo(BeNil())
			Expect(deployment.Spec.Template.Spec.Volumes[0].HostPath).To(BeNil())
			Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Image).To(Equal("curlimages/curl:8.11.1"))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Command).To(Equal([]string{"sh", "-c"}))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Args).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Args[0]).To(ContainSubstring("curl -fL"))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "MODEL_URL",
				Value: modelSource,
			}))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "MODEL_DIR",
				Value: "/models/" + cacheKeyFor(modelSource),
			}))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "MODEL_FILE",
				Value: testModelFileName,
			}))
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "EXPECT_FORMAT",
				Value: "gguf",
			}))
			Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(modelImage))
			Expect(deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy).To(Equal(corev1.PullIfNotPresent))
			Expect(deployment.Spec.Template.Spec.Containers[0].StartupProbe).NotTo(BeNil())
			Expect(deployment.Spec.Template.Spec.Containers[0].StartupProbe.PeriodSeconds).To(Equal(int32(5)))
			Expect(deployment.Spec.Template.Spec.Containers[0].StartupProbe.FailureThreshold).To(Equal(int32(120)))
			Expect(deployment.Spec.Template.Spec.Containers[0].ReadinessProbe.TimeoutSeconds).To(Equal(int32(3)))
			Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElements(
				"--model", "/models/"+cacheKeyFor(modelSource)+"/"+testModelFileName, "--ctx-size", "4096",
			))
			Expect(deployment.OwnerReferences).To(HaveLen(1))
			Expect(deployment.OwnerReferences[0].Name).To(Equal(resourceName))

			service := &corev1.Service{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, service)).To(Succeed())
			Expect(service.Spec.Ports).To(HaveLen(1))
			Expect(service.Spec.Ports[0].Port).To(Equal(int32(8080)))

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Pending"))
			Expect(model.Status.Endpoint).To(Equal("http://test-resource.default.svc.cluster.local:8080"))
			Expect(model.Status.CacheKey).To(Equal(cacheKeyFor(modelSource)))
		})

		It("uses a hostPath model cache when MODEL_CACHE_HOST_PATH is set", func() {
			cachePath := "/tmp/minillmkube-model-cache"
			Expect(os.Setenv("MODEL_CACHE_HOST_PATH", cachePath)).To(Succeed())
			defer func() {
				Expect(os.Unsetenv("MODEL_CACHE_HOST_PATH")).To(Succeed())
			}()

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Volumes).To(HaveLen(1))
			Expect(deployment.Spec.Template.Spec.Volumes[0].EmptyDir).To(BeNil())
			Expect(deployment.Spec.Template.Spec.Volumes[0].HostPath).NotTo(BeNil())
			Expect(deployment.Spec.Template.Spec.Volumes[0].HostPath.Path).To(Equal(cachePath))
			Expect(deployment.Spec.Template.Spec.Volumes[0].HostPath.Type).To(HaveValue(Equal(corev1.HostPathDirectoryOrCreate)))

			By("preparing the cache root before downloading")
			Expect(deployment.Spec.Template.Spec.InitContainers).To(HaveLen(2))
			cachePrep := deployment.Spec.Template.Spec.InitContainers[0]
			Expect(cachePrep.Name).To(Equal("cache-prep"))
			Expect(cachePrep.Command).To(Equal([]string{"chown", "100:101", "/models"}))
			Expect(cachePrep.VolumeMounts).To(ConsistOf(corev1.VolumeMount{Name: modelVolumeName, MountPath: modelMountPath}))
			Expect(cachePrep.SecurityContext).NotTo(BeNil())
			Expect(cachePrep.SecurityContext.RunAsUser).To(HaveValue(Equal(int64(0))))
			Expect(cachePrep.SecurityContext.RunAsNonRoot).To(HaveValue(BeFalse()))
			Expect(cachePrep.SecurityContext.AllowPrivilegeEscalation).To(HaveValue(BeFalse()))
			Expect(cachePrep.SecurityContext.Capabilities).NotTo(BeNil())
			Expect(cachePrep.SecurityContext.Capabilities.Drop).To(ConsistOf(corev1.Capability("ALL")))
			Expect(cachePrep.SecurityContext.Capabilities.Add).To(ConsistOf(corev1.Capability("CHOWN")))
			Expect(deployment.Spec.Template.Spec.InitContainers[1].Name).To(Equal("model-downloader"))
		})

		It("rejects invalid model sources without creating a Deployment", func() {
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Spec.Source = "http://huggingface.co/example/model.gguf"
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Failed"))
			Expect(model.Status.Conditions).To(ContainElement(WithTransform(func(condition metav1.Condition) string {
				return condition.Reason
			}, Equal("InvalidSpec"))))

			deployment := &appsv1.Deployment{}
			err = k8sClient.Get(ctx, typeNamespacedName, deployment)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("allows Hugging Face download URLs with query strings", func() {
			sourceWithQuery := modelSource + "?download=true"
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Spec.Source = sourceWithQuery
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name:  "MODEL_URL",
				Value: sourceWithQuery,
			}))
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Pending"))
		})

		It("marks only the current Deployment generation as Ready", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			observedGeneration := deployment.Generation
			deployment.Spec.Template.Annotations = map[string]string{"rollout": "new-spec"}
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())

			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Generation).To(BeNumerically(">", observedGeneration))
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			deployment.Status.UpdatedReplicas = 1
			deployment.Status.AvailableReplicas = 1
			deployment.Status.ObservedGeneration = observedGeneration
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Deploying"))

			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			deployment.Status.ObservedGeneration = deployment.Generation
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Ready"))
		})

		It("marks a deployment progress deadline as Failed", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			deployment.Status.ObservedGeneration = deployment.Generation
			deployment.Status.Replicas = 1
			deployment.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentProgressing,
				Status:  corev1.ConditionFalse,
				Reason:  "ProgressDeadlineExceeded",
				Message: "Deployment exceeded its progress deadline",
			}}
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Failed"))
			Expect(model.Status.Conditions).To(ContainElement(WithTransform(func(condition metav1.Condition) string {
				return condition.Reason
			}, Equal("ProgressDeadlineExceeded"))))
		})

		It("restores the Service type to ClusterIP", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			service := &corev1.Service{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, service)).To(Succeed())
			service.Spec.Type = corev1.ServiceTypeNodePort
			service.Spec.Ports[0].NodePort = 30080
			Expect(k8sClient.Update(ctx, service)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, service)).To(Succeed())
			Expect(service.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(service.Spec.Ports[0].NodePort).To(BeZero())
		})
		It("gives models with different sources different cache directories", func() {
			const otherSource = "https://huggingface.co/bartowski/Qwen_Qwen3-0.6B-GGUF/resolve/main/Qwen3-0.6B-Q8_0.gguf"
			otherName := types.NamespacedName{Name: resourceName + "-other", Namespace: typeNamespacedName.Namespace}
			other := &llmv1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: otherName.Name, Namespace: otherName.Namespace},
				Spec:       llmv1alpha1.ModelSpec{Source: otherSource},
			}
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, other))).To(Succeed())
			})

			for _, name := range []types.NamespacedName{typeNamespacedName, otherName} {
				_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: name})
				Expect(err).NotTo(HaveOccurred())
			}

			dirs := make([]string, 0, 2)
			for _, name := range []types.NamespacedName{typeNamespacedName, otherName} {
				deployment := &appsv1.Deployment{}
				Expect(k8sClient.Get(ctx, name, deployment)).To(Succeed())
				dirs = append(dirs, envValue(deployment.Spec.Template.Spec.InitContainers[0].Env, "MODEL_DIR"))
			}
			Expect(dirs[0]).To(Equal("/models/" + cacheKeyFor(modelSource)))
			Expect(dirs[1]).To(Equal("/models/" + cacheKeyFor(otherSource)))
			Expect(dirs[0]).NotTo(Equal(dirs[1]))
		})

		It("does not rewrite the Deployment when nothing changed", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			deployment.Status.ObservedGeneration = deployment.Generation
			deployment.Status.Replicas = 1
			deployment.Status.ReadyReplicas = 1
			deployment.Status.UpdatedReplicas = 1
			deployment.Status.AvailableReplicas = 1
			Expect(k8sClient.Status().Update(ctx, deployment)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.Phase).To(Equal("Ready"))

			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			resourceVersion := deployment.ResourceVersion

			By("touching the Model without changing anything the Deployment depends on")
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Annotations = map[string]string{"f9-regression": "true"}
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.ResourceVersion).To(Equal(resourceVersion))

			By("dropping the GPU verdict once the Model no longer asks for a GPU")
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Spec.GPU = nil
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.Spec.Template.Spec.Containers[0].Resources.Limits).NotTo(
				HaveKey(corev1.ResourceName(gpuResourceName)))
			Expect(deployment.Spec.Template.Spec.Containers[0].Args).NotTo(ContainElement("-lv"))

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(meta.FindStatusCondition(model.Status.Conditions, gpuOffloadConditionType)).To(BeNil())
		})
		It("requests a GPU when spec.gpu is set", func() {
			Expect(os.Unsetenv(gpuRuntimeClassEnv)).To(Succeed())

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Spec.GPU = &llmv1alpha1.GPUSpec{}
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())

			// No RuntimeClass comes from the environment here, so the Pod stays
			// on the cluster default and is scheduled by the resource request.
			Expect(deployment.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			Expect(deployment.Spec.Template.Spec.RuntimeClassName).To(BeNil())
			Expect(deployment.Spec.Template.Spec.Containers[0].Resources.Limits).To(HaveKeyWithValue(
				corev1.ResourceName(gpuResourceName), resource.MustParse("1"),
			))
			// The CRD defaults gpu.layers to 99 on the stored object.
			Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElements(
				"--n-gpu-layers", "99",
			))

			By("recording GPUOffload as still unknown while the Pod is not ready")
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			gpuCondition := meta.FindStatusCondition(model.Status.Conditions, gpuOffloadConditionType)
			Expect(gpuCondition).NotTo(BeNil())
			Expect(gpuCondition.Status).To(Equal(metav1.ConditionUnknown))
			Expect(gpuCondition.Reason).To(Equal(gpuReasonNotDetected))

			By("not rewriting the Deployment on the next reconcile")
			resourceVersion := deployment.ResourceVersion
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			Expect(deployment.ResourceVersion).To(Equal(resourceVersion))
		})

		It("rejects vllm with gguf weights before storing the object", func() {
			invalid := &llmv1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-runtime", Namespace: typeNamespacedName.Namespace},
				Spec: llmv1alpha1.ModelSpec{
					Source:  modelSource,
					Runtime: runtimeVLLM,
					Format:  formatGGUF,
				},
			}
			Expect(k8sClient.Create(ctx, invalid)).To(MatchError(ContainSubstring("vllm does not support gguf")))
		})

		It("resolves the runtime from the source and reports it in status", func() {
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.ResolvedRuntime).To(Equal(runtimeLlamaCpp))
			Expect(model.Status.ResolvedFormat).To(Equal(formatGGUF))
		})

		It("runs vllm for an hf:// source", func() {
			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			model.Spec.Source = testHFSource
			model.Spec.Image = ""
			Expect(k8sClient.Update(ctx, model)).To(Succeed())

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, typeNamespacedName, model)).To(Succeed())
			Expect(model.Status.ResolvedRuntime).To(Equal(runtimeVLLM))
			Expect(model.Status.ResolvedFormat).To(Equal(formatSafetensors))

			deployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, deployment)).To(Succeed())
			podSpec := deployment.Spec.Template.Spec

			Expect(podSpec.Containers[0].Image).To(Equal(vllmImage))
			Expect(podSpec.Containers[0].Args).To(ContainElements(
				"--served-model-name", resourceName, "--max-model-len", "4096",
			))
			Expect(podSpec.Containers[0].StartupProbe.FailureThreshold).To(Equal(vllmStartupFailureThreshold))
			Expect(podSpec.EnableServiceLinks).To(HaveValue(BeFalse()))

			By("giving vLLM a memory-backed /dev/shm instead of the 64MiB default")
			var shm *corev1.Volume
			for i := range podSpec.Volumes {
				if podSpec.Volumes[i].Name == sharedMemoryVolumeName {
					shm = &podSpec.Volumes[i]
				}
			}
			Expect(shm).NotTo(BeNil())
			Expect(shm.EmptyDir.Medium).To(Equal(corev1.StorageMediumMemory))
			// 1Gi request, no limit: the fallback size applies.
			Expect(shm.EmptyDir.SizeLimit.String()).To(Equal(defaultSharedMemorySize))

			By("downloading the repository with the same image")
			downloader := podSpec.InitContainers[0]
			Expect(downloader.Image).To(Equal(vllmImage))
			Expect(downloader.Args[0]).To(Equal(downloadScriptSafetensors))
			Expect(downloader.Env).To(ContainElement(corev1.EnvVar{Name: "MODEL_REPO", Value: "Qwen/Qwen3-0.6B"}))
		})
	})
})

func envValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}
