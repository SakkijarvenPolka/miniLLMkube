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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	llmv1alpha1 "github.com/SakkijarvenPolka/minillmkube/api/v1alpha1"
)

func TestRecordFailureEvent(t *testing.T) {
	recorder := events.NewFakeRecorder(8)
	reconciler := &ModelReconciler{Recorder: recorder}
	model := &llmv1alpha1.Model{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: testNamespace}}
	ctx := context.Background()

	failed := rolloutState{
		phase:   phaseFailed,
		status:  metav1.ConditionFalse,
		reason:  reasonDownloadFailed,
		message: testDownloadMessage,
	}

	t.Log("recording the first failure")

	reconciler.recordFailureEvent(ctx, model, failed, "")
	if len(recorder.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(recorder.Events))
	}
	if event := <-recorder.Events; !strings.Contains(event, reasonDownloadFailed) {
		t.Errorf("event = %q, want it to mention %q", event, reasonDownloadFailed)
	}

	t.Log("staying quiet while the reason does not change")

	reconciler.recordFailureEvent(ctx, model, failed, reasonDownloadFailed)
	if len(recorder.Events) != 0 {
		t.Errorf("events = %d, want 0", len(recorder.Events))
	}

	t.Log("emitting nothing while the Model is healthy")

	reconciler.recordFailureEvent(ctx, model, rolloutState{phase: phaseReady}, "")
	if len(recorder.Events) != 0 {
		t.Errorf("events = %d, want 0", len(recorder.Events))
	}

	t.Log("tolerating a missing recorder")

	(&ModelReconciler{}).recordFailureEvent(ctx, model, failed, "")
}

func TestPodToModelRequest(t *testing.T) {
	reconciler := &ModelReconciler{}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNamespace,
		Labels:    map[string]string{inferenceInstanceLabel: testUnitModelName},
	}}

	requests := reconciler.podToModelRequest(context.Background(), pod)
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	if want := (types.NamespacedName{Namespace: testNamespace, Name: testUnitModelName}); requests[0].NamespacedName != want {
		t.Errorf("request = %v, want %v", requests[0].NamespacedName, want)
	}

	if got := reconciler.podToModelRequest(context.Background(), &corev1.Pod{}); got != nil {
		t.Errorf("requests = %v, want nil for a Pod without the instance label", got)
	}
}

func TestIsInferencePod(t *testing.T) {
	managed := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
		inferenceInstanceLabel:  testUnitModelName,
		inferenceNameLabel:      inferenceAppName,
		inferenceManagedByLabel: inferenceManagedBy,
	}}}
	if !isInferencePod(managed) {
		t.Error("isInferencePod() = false for a Pod of this controller")
	}

	foreign := managed.DeepCopy()
	foreign.Labels[inferenceManagedByLabel] = "someone-else"
	if isInferencePod(foreign) {
		t.Error("isInferencePod() = true for a Pod managed by another controller")
	}

	if isInferencePod(&corev1.Pod{}) {
		t.Error("isInferencePod() = true for an unrelated Pod")
	}
}
