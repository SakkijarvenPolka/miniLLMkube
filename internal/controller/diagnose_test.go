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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiagnosePods(t *testing.T) {
	tests := []struct {
		name         string
		pods         []corev1.Pod
		wantReason   string
		wantContains string
	}{
		{
			name: "no pods",
		},
		{
			name: "healthy pod",
			pods: []corev1.Pod{runningInferenceTestPod("running")},
		},
		{
			name: "format mismatch in the downloader termination message",
			pods: []corev1.Pod{downloaderTestPod("misformatted",
				corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Message:  "FORMAT_MISMATCH: expected GGUF magic, got HTML",
				}})},
			wantReason:   reasonFormatMismatch,
			wantContains: "expected GGUF magic",
		},
		{
			name: "format mismatch left in the last termination state",
			pods: []corev1.Pod{downloaderTestPod("retried", corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 22, Message: testDownloadMessage},
			})},
			wantReason:   reasonDownloadFailed,
			wantContains: "404",
		},
		{
			name: "download failed with an empty message",
			pods: []corev1.Pod{downloaderTestPod("no-message", corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 6},
			})},
			wantReason:   reasonDownloadFailed,
			wantContains: "exited with code 6",
		},
		{
			name: "image pull failure on the inference container",
			pods: []corev1.Pod{waitingTestPod(inferenceContainerName, "ErrImagePull",
				"Back-off pulling image \"nope\"")},
			wantReason:   reasonImagePullFailed,
			wantContains: "nope",
		},
		{
			name:         "image pull backoff on the downloader",
			pods:         []corev1.Pod{waitingTestPod(downloaderContainerName, "ImagePullBackOff", "")},
			wantReason:   reasonImagePullFailed,
			wantContains: downloaderContainerName,
		},
		{
			name: "gpu starved pod",
			pods: []corev1.Pod{unschedulableTestPod(
				"0/1 nodes are available: 1 Insufficient nvidia.com/gpu.")},
			wantReason:   reasonInsufficientGPU,
			wantContains: "Insufficient",
		},
		{
			name: "pod unschedulable for another reason",
			pods: []corev1.Pod{unschedulableTestPod(
				"0/1 nodes are available: 1 node(s) had untolerated taint.")},
			wantReason:   reasonUnschedulable,
			wantContains: "taint",
		},
		{
			name:         "crash looping runtime",
			pods:         []corev1.Pod{crashingTestPod("invalid argument --bogus")},
			wantReason:   reasonRuntimeCrashing,
			wantContains: "exit code 1",
		},
		{
			name: "format mismatch outranks a download failure",
			pods: []corev1.Pod{
				downloaderTestPod("failed", corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 22, Message: testDownloadMessage},
				}),
				downloaderTestPod("format", corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{Message: "FORMAT_MISMATCH: bad magic"},
				}),
			},
			wantReason:   reasonFormatMismatch,
			wantContains: "bad magic",
		},
		{
			name: "download failure outranks an unschedulable pod",
			pods: []corev1.Pod{
				unschedulableTestPod("0/1 nodes are available: 1 Insufficient cpu."),
				downloaderTestPod("failed", corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 22, Message: "timeout"},
				}),
			},
			wantReason:   reasonDownloadFailed,
			wantContains: "timeout",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := diagnosePods(test.pods)
			if test.wantReason == "" {
				if got != nil {
					t.Fatalf("diagnosePods() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("diagnosePods() = nil, want reason %q", test.wantReason)
			}
			if got.reason != test.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, test.wantReason)
			}
			if !strings.Contains(got.message, test.wantContains) {
				t.Errorf("message = %q, want it to contain %q", got.message, test.wantContains)
			}
		})
	}
}

func TestDiagnosePodsTruncatesLongMessages(t *testing.T) {
	message := "FORMAT_MISMATCH: " + strings.Repeat("detail ", 400)
	got := diagnosePods([]corev1.Pod{downloaderTestPod("long", corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{Message: message},
	})})

	if got == nil {
		t.Fatal("diagnosePods() = nil, want a diagnosis")
	}
	if len(got.message) > diagnosisMessageLimit+len("...") {
		t.Errorf("message length = %d, want at most %d", len(got.message), diagnosisMessageLimit+len("..."))
	}
	if !strings.HasSuffix(got.message, "...") {
		t.Errorf("message = %q, want a truncation marker", got.message)
	}
}

func downloaderTestPod(name string, state corev1.ContainerState) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  downloaderContainerName,
				State: state,
			}},
		},
	}
}

func runningInferenceTestPod(name string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  inferenceContainerName,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				Ready: true,
			}},
		},
	}
}

func waitingTestPod(container, reason, message string) corev1.Pod {
	status := corev1.ContainerStatus{
		Name: container,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
			Reason:  reason,
			Message: message,
		}},
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: container + "-waiting"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	if container == downloaderContainerName {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
	} else {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
	}
	return pod
}

func unschedulableTestPod(message string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unschedulable"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  "Unschedulable",
				Message: message,
			}},
		},
	}
}

func crashingTestPod(message string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crashing"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: inferenceContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason:  "CrashLoopBackOff",
					Message: message,
				}},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
				},
			}},
		},
	}
}
