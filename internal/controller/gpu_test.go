package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	testCUDADevice      = "CUDA0"
	testPodName         = "qwen-0"
	testCPUFallbackCase = "cpu fallback"
)

func TestParseGPUDevice(t *testing.T) {
	tests := []struct {
		name      string
		logText   string
		wantDev   string
		wantFound bool
	}{
		{
			name:      "cuda device",
			logText:   "load_tensors: offloaded 99/99 layers to GPU\nllama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 4090, 24000 MiB free)\nllama_model_load: model size = 500 MiB",
			wantDev:   testCUDADevice,
			wantFound: true,
		},
		{
			name:      "second gpu",
			logText:   "llama_prepare_model_devices: using device CUDA1",
			wantDev:   "CUDA1",
			wantFound: true,
		},
		{
			name:      testCPUFallbackCase,
			logText:   "ggml_cuda_init: failed to initialize CUDA: no CUDA-capable device is detected\nllama_prepare_model_devices: using device CPU",
			wantDev:   gpuDeviceCPU,
			wantFound: true,
		},
		{
			name:      "no device line",
			logText:   "llama_model_loader: loaded meta data\nserver is listening on http://0.0.0.0:8080",
			wantFound: false,
		},
		{
			name:      "empty log",
			logText:   "",
			wantFound: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device, found := parseGPUDevice(test.logText)
			if device != test.wantDev || found != test.wantFound {
				t.Fatalf("parseGPUDevice() = (%q, %v), want (%q, %v)",
					device, found, test.wantDev, test.wantFound)
			}
			if !found {
				return
			}
			line := gpuDeviceLine(test.logText)
			if line == "" {
				t.Fatalf("gpuDeviceLine() returned no line while the device was found")
			}
			if !strings.Contains(line, device) {
				t.Fatalf("gpuDeviceLine() = %q, does not quote device %q", line, device)
			}
		})
	}
}

func TestGPULogStatus(t *testing.T) {
	tests := []struct {
		name        string
		observation gpuObservation
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "offload detected",
			observation: gpuObservation{
				device: testCUDADevice,
				line:   "llama_prepare_model_devices: using device CUDA0",
				found:  true,
			},
			wantStatus:  metav1.ConditionTrue,
			wantReason:  gpuReasonDetected,
			wantMessage: "Running on CUDA0: llama_prepare_model_devices: using device CUDA0",
		},
		{
			name: testCPUFallbackCase,
			observation: gpuObservation{
				device: gpuDeviceCPU,
				line:   "llama_prepare_model_devices: using device CPU",
				found:  true,
			},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  gpuReasonCPUFallback,
			wantMessage: "llama.cpp fell back to the CPU: llama_prepare_model_devices: using device CPU",
		},
		{
			name:        "no signal",
			observation: gpuObservation{},
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  gpuReasonNotDetected,
			wantMessage: "The inference log has no device line yet",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			condition := gpuLogStatus(test.observation)
			if condition.Type != gpuOffloadConditionType {
				t.Fatalf("condition type = %q, want %q", condition.Type, gpuOffloadConditionType)
			}
			if condition.Status != test.wantStatus || condition.Reason != test.wantReason {
				t.Fatalf("condition = %s/%s, want %s/%s",
					condition.Status, condition.Reason, test.wantStatus, test.wantReason)
			}
			if condition.Message != test.wantMessage {
				t.Fatalf("message = %q, want %q", condition.Message, test.wantMessage)
			}
		})
	}
}

func TestObserveGPULoad(t *testing.T) {
	cudaLog := "llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 4090)"
	cpuLog := "ggml_cuda_init: failed to initialize CUDA\nllama_prepare_model_devices: using device CPU"

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core v1 to scheme: %v", err)
	}
	if err := llmv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add llm to scheme: %v", err)
	}

	newPod := func(uid types.UID) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testPodName,
				Namespace: testNamespace,
				UID:       uid,
				Labels:    labelsFor(testModel(llmv1alpha1.ModelSpec{})),
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}

	t.Run("no running pod", func(t *testing.T) {
		reconciler := &ModelReconciler{
			Client: fakeClientWith(t, scheme, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: testNamespace},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			}),
			readPodLog: func(context.Context, *corev1.Pod) (string, error) {
				t.Fatal("the log must not be read without a running pod")
				return "", nil
			},
		}

		observation, err := reconciler.observeGPULoad(context.Background(), testModel(llmv1alpha1.ModelSpec{}))
		if err != nil {
			t.Fatalf("observeGPULoad() error = %v", err)
		}
		if observation.found {
			t.Fatalf("observation = %+v, want no signal", observation)
		}
	})

	t.Run("reads once per pod", func(t *testing.T) {
		reads := 0
		reconciler := &ModelReconciler{
			Client: fakeClientWith(t, scheme, newPod("uid-1")),
			readPodLog: func(context.Context, *corev1.Pod) (string, error) {
				reads++
				return cudaLog, nil
			},
		}
		model := testModel(llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}})

		for range 3 {
			observation, err := reconciler.observeGPULoad(context.Background(), model)
			if err != nil {
				t.Fatalf("observeGPULoad() error = %v", err)
			}
			if observation.device != testCUDADevice || !observation.found {
				t.Fatalf("observation = %+v, want CUDA0", observation)
			}
		}
		if reads != 1 {
			t.Fatalf("log reads = %d, want 1 (the observation is cached per pod)", reads)
		}
	})

	t.Run("a replaced pod is read again", func(t *testing.T) {
		reads := 0
		reconciler := &ModelReconciler{
			Client: fakeClientWith(t, scheme, newPod("uid-1")),
			readPodLog: func(context.Context, *corev1.Pod) (string, error) {
				reads++
				return cpuLog, nil
			},
		}
		model := testModel(llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}})

		if _, err := reconciler.observeGPULoad(context.Background(), model); err != nil {
			t.Fatalf("observeGPULoad() error = %v", err)
		}

		replaced := newPod("uid-1")
		if err := reconciler.Get(context.Background(), client.ObjectKeyFromObject(replaced), replaced); err != nil {
			t.Fatalf("get pod: %v", err)
		}
		replaced.UID = "uid-2"
		if err := reconciler.Update(context.Background(), replaced); err != nil {
			t.Fatalf("replace pod: %v", err)
		}

		observation, err := reconciler.observeGPULoad(context.Background(), model)
		if err != nil {
			t.Fatalf("observeGPULoad() error = %v", err)
		}
		if observation.device != gpuDeviceCPU {
			t.Fatalf("device = %q, want CPU", observation.device)
		}
		if reads != 2 {
			t.Fatalf("log reads = %d, want 2", reads)
		}
	})

	t.Run("log read failure is reported", func(t *testing.T) {
		reconciler := &ModelReconciler{
			Client: fakeClientWith(t, scheme, newPod("uid-1")),
			readPodLog: func(context.Context, *corev1.Pod) (string, error) {
				return "", errors.New("forbidden")
			},
		}

		if _, err := reconciler.observeGPULoad(context.Background(), testModel(llmv1alpha1.ModelSpec{})); err == nil {
			t.Fatal("observeGPULoad() error = nil, want the log read failure")
		}
	})
}

func TestGPUConditions(t *testing.T) {
	cudaLog := "llama_prepare_model_devices: using device CUDA0"
	cpuLog := "llama_prepare_model_devices: using device CPU"
	ready := rolloutState{phase: phaseReady}

	tests := []struct {
		name       string
		spec       llmv1alpha1.ModelSpec
		state      rolloutState
		logText    string
		wantNil    bool
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:    "cpu model has no gpu condition",
			spec:    llmv1alpha1.ModelSpec{},
			state:   ready,
			wantNil: true,
		},
		{
			name:       "waiting for the pod",
			spec:       llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}},
			state:      rolloutState{phase: phaseDeploying},
			wantStatus: metav1.ConditionUnknown,
			wantReason: gpuReasonNotDetected,
		},
		{
			name:       "offload detected",
			spec:       llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}},
			state:      ready,
			logText:    cudaLog,
			wantStatus: metav1.ConditionTrue,
			wantReason: gpuReasonDetected,
		},
		{
			name:       testCPUFallbackCase,
			spec:       llmv1alpha1.ModelSpec{GPU: &llmv1alpha1.GPUSpec{Count: 1, Layers: 99}},
			state:      ready,
			logText:    cpuLog,
			wantStatus: metav1.ConditionFalse,
			wantReason: gpuReasonCPUFallback,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("add core v1 to scheme: %v", err)
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testPodName,
					Namespace: testNamespace,
					UID:       "uid-1",
					Labels:    labelsFor(testModel(llmv1alpha1.ModelSpec{})),
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			reconciler := &ModelReconciler{
				Client: fakeClientWith(t, scheme, pod),
				readPodLog: func(context.Context, *corev1.Pod) (string, error) {
					return test.logText, nil
				},
			}

			conditions := reconciler.gpuConditions(context.Background(), testModel(test.spec), test.state)
			if test.wantNil {
				if len(conditions) != 0 {
					t.Fatalf("conditions = %+v, want none", conditions)
				}
				return
			}
			if len(conditions) != 1 {
				t.Fatalf("conditions = %+v, want exactly one", conditions)
			}
			if conditions[0].Status != test.wantStatus || conditions[0].Reason != test.wantReason {
				t.Fatalf("condition = %s/%s, want %s/%s",
					conditions[0].Status, conditions[0].Reason, test.wantStatus, test.wantReason)
			}
		})
	}
}

func fakeClientWith(t *testing.T, scheme *runtime.Scheme, objects ...client.Object) client.Client {
	t.Helper()
	return fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestInferencePodLog(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPodName, Namespace: testNamespace}}

	t.Run("without a clientset", func(t *testing.T) {
		controller := &ModelReconciler{}
		if _, err := controller.inferencePodLog(context.Background(), pod); err == nil {
			t.Fatal("inferencePodLog() error = nil, want a missing client error")
		}
	})

	t.Run("reads the inference container", func(t *testing.T) {
		controller := &ModelReconciler{Clientset: fake.NewClientset(pod)}
		logText, err := controller.inferencePodLog(context.Background(), pod)
		if err != nil {
			t.Fatalf("inferencePodLog() error = %v", err)
		}
		if logText == "" {
			t.Fatal("inferencePodLog() returned an empty log")
		}
	})
}
