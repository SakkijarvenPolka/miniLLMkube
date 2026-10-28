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
	"fmt"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
)

const (
	// diagnosisMessageLimit bounds the text copied into a status condition. A
	// termination message may be several KiB; a condition should stay readable
	// and fit the 512 byte guidance of E34.
	diagnosisMessageLimit = 512

	reasonFormatMismatch  = "FormatMismatch"
	reasonDownloadFailed  = "DownloadFailed"
	reasonImagePullFailed = "ImagePullFailed"
	reasonInsufficientGPU = "InsufficientGPU"
	reasonUnschedulable   = "Unschedulable"
	reasonRuntimeCrashing = "RuntimeCrashing"

	// formatMismatchPrefix is what the download scripts write before a format
	// problem, so it identifies the failure without parsing prose.
	formatMismatchPrefix = "FORMAT_MISMATCH:"

	podScheduledCondition = "PodScheduled"
	unschedulableReason   = "Unschedulable"
	crashLoopReason       = "CrashLoopBackOff"
)

// diagnosis is the single most useful explanation of why the Pods of a Model
// are not serving.
type diagnosis struct {
	reason  string
	message string
}

// imagePullReasons are the container waiting reasons that mean the image could
// not be pulled. They are identical for init containers and containers.
var imagePullReasons = map[string]struct{}{
	"ErrImagePull":     {},
	"ImagePullBackOff": {},
	"InvalidImageName": {},
}

// diagnosePods explains the first failure of a Model's Pods by priority. Pods
// are cheap to read and every signal below is already in their status, so no
// extra API call is needed beyond the List itself.
//
// Priority (first match wins): format mismatch, failed download, image pull,
// GPU starvation, any other unschedulable Pod, crash looping runtime.
func diagnosePods(pods []corev1.Pod) *diagnosis {
	var (
		found    *diagnosis
		priority int
	)
	consider := func(candidatePriority int, candidate diagnosis) {
		if found != nil && priority <= candidatePriority {
			return
		}
		candidate.message = truncateDiagnosisMessage(candidate.message)
		found = &candidate
		priority = candidatePriority
	}

	for i := range pods {
		pod := &pods[i]
		if candidate, candidatePriority, ok := downloaderDiagnosis(pod); ok {
			consider(candidatePriority, candidate)
		}
		if candidate, ok := imagePullDiagnosis(pod); ok {
			consider(3, candidate)
		}
		if candidate, candidatePriority, ok := schedulingDiagnosis(pod); ok {
			consider(candidatePriority, candidate)
		}
		if candidate, ok := crashDiagnosis(pod); ok {
			consider(6, candidate)
		}
	}
	return found
}

// downloaderDiagnosis reports how the model download failed: a format problem
// takes precedence over a generic download error.
func downloaderDiagnosis(pod *corev1.Pod) (diagnosis, int, bool) {
	status, ok := containerStatus(pod, downloaderContainerName)
	if !ok {
		return diagnosis{}, 0, false
	}
	// LastTerminationState is historical. A current running or successful
	// termination must not resurrect an old download error.
	if status.State.Running != nil {
		return diagnosis{}, 0, false
	}
	terminated := status.State.Terminated
	if terminated == nil && status.State.Waiting != nil {
		terminated = status.LastTerminationState.Terminated
	}
	if terminated == nil {
		return diagnosis{}, 0, false
	}
	message := strings.TrimSpace(terminated.Message)
	if strings.HasPrefix(message, formatMismatchPrefix) {
		return diagnosis{reason: reasonFormatMismatch, message: message}, 1, true
	}
	if terminated.ExitCode != 0 {
		if message == "" {
			message = fmt.Sprintf("%s exited with code %d", downloaderContainerName, terminated.ExitCode)
		}
		return diagnosis{reason: reasonDownloadFailed, message: message}, 2, true
	}
	return diagnosis{}, 0, false
}

// imagePullDiagnosis reports an image that the kubelet could not pull or that
// does not name a valid reference.
func imagePullDiagnosis(pod *corev1.Pod) (diagnosis, bool) {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)

	for _, status := range statuses {
		if status.State.Waiting == nil {
			continue
		}
		if _, failing := imagePullReasons[status.State.Waiting.Reason]; !failing {
			continue
		}
		message := status.State.Waiting.Message
		if message == "" {
			message = fmt.Sprintf("%s: %s", status.Name, status.State.Waiting.Reason)
		}
		return diagnosis{reason: reasonImagePullFailed, message: message}, true
	}
	return diagnosis{}, false
}

// schedulingDiagnosis reports a Pod the scheduler refused to place. A Pod that
// cannot get a GPU is called out separately, because the fix is a capacity
// change rather than a placement change.
func schedulingDiagnosis(pod *corev1.Pod) (diagnosis, int, bool) {
	for _, condition := range pod.Status.Conditions {
		if condition.Type != podScheduledCondition || condition.Status != corev1.ConditionFalse {
			continue
		}
		if condition.Reason != unschedulableReason {
			continue
		}
		message := condition.Message
		if message == "" {
			message = "the scheduler could not place the Pod"
		}
		if strings.Contains(message, gpuResourceName) {
			return diagnosis{reason: reasonInsufficientGPU, message: message}, 4, true
		}
		return diagnosis{reason: reasonUnschedulable, message: message}, 5, true
	}
	return diagnosis{}, 0, false
}

// crashDiagnosis reports an inference container that starts and crashes
// repeatedly. The exit code of the previous run is what tells a bad flag from a
// missing library.
func crashDiagnosis(pod *corev1.Pod) (diagnosis, bool) {
	status, ok := containerStatus(pod, inferenceContainerName)
	if !ok || status.State.Waiting == nil || status.State.Waiting.Reason != crashLoopReason {
		return diagnosis{}, false
	}

	message := status.State.Waiting.Message
	if message == "" {
		message = "the inference container is crash looping"
	}
	if last := status.LastTerminationState.Terminated; last != nil {
		message = fmt.Sprintf("%s (last exit code %d)", message, last.ExitCode)
	}
	return diagnosis{reason: reasonRuntimeCrashing, message: message}, true
}

// containerStatus finds the status of one container, init containers included.
func containerStatus(pod *corev1.Pod, name string) (corev1.ContainerStatus, bool) {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name == name {
			return status, true
		}
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}

// truncateDiagnosisMessage cuts a message to diagnosisMessageLimit bytes on a
// rune boundary, so a status update never carries a multi-KiB termination
// message.
func truncateDiagnosisMessage(message string) string {
	const marker = "..."
	if utf8.ValidString(message) && len(message) <= diagnosisMessageLimit {
		return message
	}
	if !utf8.ValidString(message) {
		message = strings.ToValidUTF8(message, "�")
	}
	if len(message) <= diagnosisMessageLimit {
		return message
	}
	// Reserve bytes for the marker; trim at a rune boundary and ensure the
	// complete result, including marker, is strictly within 512 bytes.
	cut := diagnosisMessageLimit - len(marker)
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return strings.TrimSpace(message[:cut]) + marker
}
