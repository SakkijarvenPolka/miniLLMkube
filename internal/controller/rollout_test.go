package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEvaluateRolloutScaledToZero(t *testing.T) {
	tests := []struct {
		name       string
		desired    int32
		replicas   int32
		wantPhase  string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "terminating replica keeps the gpu",
			desired:    0,
			replicas:   1,
			wantPhase:  phaseScaledToZero,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonScalingDown,
		},
		{
			name:       "idle model",
			desired:    0,
			replicas:   0,
			wantPhase:  phaseScaledToZero,
			wantStatus: metav1.ConditionFalse,
			wantReason: reasonScaledToZero,
		},
		{
			name:       "ready model",
			desired:    1,
			replicas:   1,
			wantPhase:  phaseReady,
			wantStatus: metav1.ConditionTrue,
			wantReason: "DeploymentReady",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deployment := &appsv1.Deployment{}
			deployment.Generation = 1
			deployment.Status.ObservedGeneration = 1
			deployment.Status.Replicas = test.replicas
			deployment.Status.UpdatedReplicas = test.replicas
			deployment.Status.AvailableReplicas = test.replicas
			deployment.Status.ReadyReplicas = test.replicas

			state := evaluateRollout(deployment, test.desired)
			if state.phase != test.wantPhase || state.status != test.wantStatus || state.reason != test.wantReason {
				t.Fatalf("evaluateRollout() = %q/%s/%q, want %q/%s/%q",
					state.phase, state.status, state.reason, test.wantPhase, test.wantStatus, test.wantReason)
			}
		})
	}
}
