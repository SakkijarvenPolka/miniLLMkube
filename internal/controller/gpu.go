package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

const (
	// gpuOffloadConditionType records what llama.cpp actually did with the GPU.
	// Requesting a GPU is a request; this condition is the observation.
	gpuOffloadConditionType = "GPUOffload"

	gpuLogLimitBytes = int64(512 * 1024)

	gpuDeviceCUDA = "CUDA"
	gpuDeviceCPU  = "CPU"

	// scaledToZeroGPUOffloadMessage replaces the device-line verdict while no
	// inference Pod exists.
	scaledToZeroGPUOffloadMessage = "Scaled to zero: there is no inference Pod to inspect"

	gpuReasonDetected    = "OffloadDetected"
	gpuReasonCPUFallback = "CPUFallback"
	gpuReasonNotDetected = "NotDetected"
)

// gpuDeviceRe matches the line llama.cpp prints once the model is loaded and the
// device split is decided. It is the only reliable signal: the image and the
// resource request say what was asked for, not what happened.
var gpuDeviceRe = regexp.MustCompile(`llama_prepare_model_devices: using device (CUDA[0-9]+|CPU)`)

// gpuObservation is the outcome of reading one inference log.
type gpuObservation struct {
	device string // "CUDA0", "CPU", or empty when no signal was present
	line   string // the log line the device was read from
	found  bool
}

// gpuScan remembers the log read done for one Pod, so a reconcile loop does not
// re-read a Pod log with a known device. A replacement Pod or a container
// restart causes another scan; missing device lines remain retryable.
type gpuScan struct {
	podUID       types.UID
	restartCount int32
	observation  gpuObservation
}

// parseGPUDevice extracts the device llama.cpp reports having prepared.
func parseGPUDevice(logText string) (string, bool) {
	match := gpuDeviceRe.FindStringSubmatch(logText)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// gpuDeviceLine returns the log line the device was read from, so the status
// condition can quote the evidence verbatim.
func gpuDeviceLine(logText string) string {
	location := gpuDeviceRe.FindStringIndex(logText)
	if location == nil {
		return ""
	}

	start := strings.LastIndexByte(logText[:location[0]], '\n') + 1
	end := strings.IndexByte(logText[location[1]:], '\n')
	if end < 0 {
		end = len(logText)
	} else {
		end += location[1]
	}
	return strings.TrimSpace(logText[start:end])
}

// gpuLogStatus maps one observation to the GPUOffload condition. The reasons are
// what a user reads first: "OffloadDetected" means the GPU is in use,
// "CPUFallback" means the operator asked for a GPU and llama.cpp ignored it.
func gpuLogStatus(observation gpuObservation) metav1.Condition {
	switch {
	case !observation.found:
		return metav1.Condition{
			Type:    gpuOffloadConditionType,
			Status:  metav1.ConditionUnknown,
			Reason:  gpuReasonNotDetected,
			Message: truncateDiagnosisMessage("The inference log has no device line yet"),
		}
	case strings.HasPrefix(observation.device, gpuDeviceCUDA):
		return metav1.Condition{
			Type:    gpuOffloadConditionType,
			Status:  metav1.ConditionTrue,
			Reason:  gpuReasonDetected,
			Message: truncateDiagnosisMessage(fmt.Sprintf("Running on %s: %s", observation.device, observation.line)),
		}
	default:
		return metav1.Condition{
			Type:    gpuOffloadConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  gpuReasonCPUFallback,
			Message: truncateDiagnosisMessage(fmt.Sprintf("llama.cpp fell back to the CPU: %s", observation.line)),
		}
	}
}

// runningInferencePod returns the Pod currently serving the Model, or nil while
// none is running.
func (r *ModelReconciler) runningInferencePod(
	ctx context.Context,
	model *llmv1alpha1.Model,
) (*corev1.Pod, error) {
	pods, err := r.currentInferencePods(ctx, model)
	if err != nil {
		return nil, err
	}
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning && pods[i].DeletionTimestamp == nil {
			return &pods[i], nil
		}
	}
	return nil, nil
}

// inferencePodLog reads the inference container log from the start of the Pod.
//
// The device line is printed while the model loads, at the very beginning of
// the log, so a tail window would drift past it as soon as a busy server logs
// more than the window. Reading from the start with a byte cap keeps the read
// bounded while still covering the startup lines.
func (r *ModelReconciler) inferencePodLog(ctx context.Context, pod *corev1.Pod) (string, error) {
	if r.Clientset == nil {
		return "", errors.New("pod log client is not configured")
	}

	limitBytes := gpuLogLimitBytes
	opts := &corev1.PodLogOptions{
		Container:  inferenceContainerName,
		LimitBytes: &limitBytes,
	}
	if !pod.CreationTimestamp.IsZero() {
		sinceSeconds := max(0, int64(time.Since(pod.CreationTimestamp.Time).Seconds()))
		opts.SinceSeconds = &sinceSeconds
	}
	stream, err := r.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("open log stream for pod %s: %w", pod.Name, err)
	}
	defer func() { _ = stream.Close() }()

	data, err := io.ReadAll(io.LimitReader(stream, gpuLogLimitBytes))
	if err != nil {
		return "", fmt.Errorf("read log of pod %s: %w", pod.Name, err)
	}
	return string(data), nil
}

// readInferenceLog reads the inference container log, through the API server in
// production and through the test seam when one is installed.
func (r *ModelReconciler) readInferenceLog(ctx context.Context, pod *corev1.Pod) (string, error) {
	if r.readPodLog != nil {
		return r.readPodLog(ctx, pod)
	}
	return r.inferencePodLog(ctx, pod)
}

func inferenceRestartCount(pod *corev1.Pod) int32 {
	status, ok := containerStatus(pod, inferenceContainerName)
	if !ok {
		return 0
	}
	return status.RestartCount
}

// observeGPULoad reads the inference log of the running Pod once and remembers
// what it saw. A Model without a running Pod yields an empty observation: there
// is nothing to read yet, which is not an error.
func (r *ModelReconciler) observeGPULoad(
	ctx context.Context,
	model *llmv1alpha1.Model,
) (gpuObservation, error) {
	pod, err := r.runningInferencePod(ctx, model)
	if err != nil {
		return gpuObservation{}, err
	}
	if pod == nil {
		return gpuObservation{}, nil
	}

	key := client.ObjectKeyFromObject(model)
	r.gpuMu.Lock()
	cached, seen := r.gpuScans[key]
	r.gpuMu.Unlock()
	restartCount := inferenceRestartCount(pod)
	if seen && cached.podUID == pod.UID && cached.restartCount == restartCount && cached.observation.found {
		return cached.observation, nil
	}

	logText, err := r.readInferenceLog(ctx, pod)
	if err != nil {
		return gpuObservation{}, err
	}

	observation := gpuObservation{line: gpuDeviceLine(logText)}
	observation.device, observation.found = parseGPUDevice(logText)

	// Do not cache a successful read with no device line: a later reconcile may
	// observe the line after startup. On read failure, return before this block,
	// preserving any previous good cache entry.
	if observation.found {
		r.gpuMu.Lock()
		if r.gpuScans == nil {
			r.gpuScans = make(map[types.NamespacedName]gpuScan)
		}
		r.gpuScans[key] = gpuScan{podUID: pod.UID, restartCount: restartCount, observation: observation}
		r.gpuMu.Unlock()
	}

	return observation, nil
}

// gpuConditions returns the GPUOffload condition to record, or nil when the
// Model does not ask for a GPU (nothing was rendered to verify).
func (r *ModelReconciler) gpuConditions(
	ctx context.Context,
	model *llmv1alpha1.Model,
	state rolloutState,
) []metav1.Condition {
	if !wantsGPU(model) {
		return nil
	}

	// Nothing runs at replicas 0, so the log-based verdict is reset instead of
	// keeping the offload state of a Pod that no longer exists.
	if replicasFor(model) == 0 {
		return []metav1.Condition{{
			Type:               gpuOffloadConditionType,
			Status:             metav1.ConditionUnknown,
			Reason:             gpuReasonNotDetected,
			ObservedGeneration: model.Generation,
			Message:            truncateDiagnosisMessage(scaledToZeroGPUOffloadMessage),
		}}
	}

	condition := metav1.Condition{
		Type:               gpuOffloadConditionType,
		ObservedGeneration: model.Generation,
	}
	backend, _, backendErr := resolveBackend(model)
	if backendErr != nil || backend.Name() != runtimeLlamaCpp {
		condition.Status = metav1.ConditionUnknown
		condition.Reason = gpuReasonNotDetected
		condition.Message = "GPU offload detection is not supported for this runtime"
		if backendErr != nil {
			condition.Message = fmt.Sprintf("GPU offload detection is unavailable: %s", backendErr)
		}
		condition.Message = truncateDiagnosisMessage(condition.Message)
		return []metav1.Condition{condition}
	}
	if state.phase != phaseReady {
		// The device line exists only once llama.cpp has loaded the model; a Pod
		// that has not rolled out yet has nothing to inspect.
		condition.Status = metav1.ConditionUnknown
		condition.Reason = gpuReasonNotDetected
		condition.Message = truncateDiagnosisMessage("Waiting for the inference Pod to become ready")
		return []metav1.Condition{condition}
	}

	observation, err := r.observeGPULoad(ctx, model)
	if err != nil {
		// A log that cannot be read is reported, not guessed: leaving the
		// condition Unknown keeps "we could not tell" distinct from "it runs on
		// the CPU". Logging it keeps the error out of the requeue path, which
		// would only repeat the same read.
		logf.FromContext(ctx).Error(err, "Failed to read the inference log for GPU detection",
			"name", model.Name)
		condition.Status = metav1.ConditionUnknown
		condition.Reason = gpuReasonNotDetected
		condition.Message = truncateDiagnosisMessage(fmt.Sprintf("Could not read the inference log: %s", err.Error()))
		return []metav1.Condition{condition}
	}

	result := gpuLogStatus(observation)
	result.ObservedGeneration = model.Generation
	return []metav1.Condition{result}
}
