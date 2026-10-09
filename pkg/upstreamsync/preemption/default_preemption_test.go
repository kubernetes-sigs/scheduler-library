// Copyright The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package preemption

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"k8s.io/apimachinery/pkg/types"
	componentmetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/testutil"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

type mockMutableSharedLister struct {
	fwk.MutableSnapshotSharedLister

	startMutationsErr error
	endMutationsErr   error
	startCalled       bool
	endCalled         bool
}

func (m *mockMutableSharedLister) StartMutations() error {
	m.startCalled = true
	return m.startMutationsErr
}

func (m *mockMutableSharedLister) EndMutations() error {
	m.endCalled = true
	return m.endMutationsErr
}

type mockHandle struct {
	fwk.Handle

	mutableLister fwk.MutableSnapshotSharedLister
}

func (m *mockHandle) MutableSnapshotSharedLister() fwk.MutableSnapshotSharedLister {
	return m.mutableLister
}

type mockPodGroupEvaluator struct {
	result *fwk.PodGroupPostFilterResult
	status *fwk.Status
	called bool
}

func (m *mockPodGroupEvaluator) Preempt(_ context.Context, _ fwk.PodGroupInfo, _ fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
	m.called = true
	return m.result, m.status
}

func TestDefaultPreemption_PodGroupPostFilter(t *testing.T) {
	metrics.InitMetrics()
	testRegistry := componentmetrics.NewKubeRegistry()
	testRegistry.MustRegister(metrics.WorkloadPreemptionAttempts)

	expectedResult := &fwk.PodGroupPostFilterResult{
		NominatingInfos: map[types.NamespacedName]*fwk.NominatingInfo{
			{Namespace: "default", Name: "p1"}: {NominatingMode: fwk.ModeOverride, NominatedNodeName: "node1"},
		},
	}

	tests := []struct {
		name               string
		startMutationsErr  error
		endMutationsErr    error
		evaluatorResult    *fwk.PodGroupPostFilterResult
		evaluatorStatus    *fwk.Status
		expectedResult     *fwk.PodGroupPostFilterResult
		expectedStatus     *fwk.Status
		wantEvaluatorCalls bool
		wantEndCalled      bool
	}{
		{
			name:               "success with status message prefixes message",
			evaluatorResult:    expectedResult,
			evaluatorStatus:    fwk.NewStatus(fwk.Success, "found a placement for podgroup, preempting 1 victims"),
			expectedResult:     expectedResult,
			expectedStatus:     fwk.NewStatus(fwk.Success, "pod group preemption: found a placement for podgroup, preempting 1 victims"),
			wantEvaluatorCalls: true,
			wantEndCalled:      true,
		},
		{
			name:               "success with empty status message preserves empty message",
			evaluatorResult:    expectedResult,
			evaluatorStatus:    fwk.NewStatus(fwk.Success),
			expectedResult:     expectedResult,
			expectedStatus:     fwk.NewStatus(fwk.Success),
			wantEvaluatorCalls: true,
			wantEndCalled:      true,
		},
		{
			name:               "unschedulable with status message prefixes message",
			evaluatorStatus:    fwk.NewStatus(fwk.Unschedulable, "No preemption victims found for incoming preemptor"),
			expectedStatus:     fwk.NewStatus(fwk.Unschedulable, "pod group preemption: No preemption victims found for incoming preemptor"),
			wantEvaluatorCalls: true,
			wantEndCalled:      true,
		},
		{
			name:               "unschedulable with empty status message preserves empty message",
			evaluatorStatus:    fwk.NewStatus(fwk.Unschedulable),
			expectedStatus:     fwk.NewStatus(fwk.Unschedulable),
			wantEvaluatorCalls: true,
			wantEndCalled:      true,
		},
		{
			name:               "error when StartMutations fails",
			startMutationsErr:  errors.New("start error"),
			evaluatorStatus:    fwk.NewStatus(fwk.Success),
			expectedStatus:     fwk.AsStatus(errors.New("pod group preemption: failed to start mutations: start error")),
			wantEvaluatorCalls: false,
			wantEndCalled:      false,
		},
		{
			name:               "error when EndMutations fails overrides status",
			endMutationsErr:    errors.New("end error"),
			evaluatorResult:    expectedResult,
			evaluatorStatus:    fwk.NewStatus(fwk.Success, "found a placement"),
			expectedResult:     expectedResult,
			expectedStatus:     fwk.AsStatus(errors.New("pod group preemption: failed to end mutations: end error")),
			wantEvaluatorCalls: true,
			wantEndCalled:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			mutableLister := &mockMutableSharedLister{
				startMutationsErr: tt.startMutationsErr,
				endMutationsErr:   tt.endMutationsErr,
			}
			fh := &mockHandle{
				mutableLister: mutableLister,
			}
			evaluator := &mockPodGroupEvaluator{
				result: tt.evaluatorResult,
				status: tt.evaluatorStatus,
			}
			dp := &DefaultPreemption{
				fh:                fh,
				podGroupEvaluator: evaluator,
			}

			pgInfo := makePodGroupPreemptor(st.MakePodGroup().Name("pg").Obj(), nil)
			schedulingFunc := func(ctx context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
				return nil, fwk.NewStatus(fwk.Success)
			}

			metricLabel := tt.expectedStatus.Code().String()
			beforeMetric, _ := testutil.GetCounterMetricValue(metrics.WorkloadPreemptionAttempts.WithLabelValues(metricLabel))

			gotResult, gotStatus := dp.PodGroupPostFilter(ctx, framework.NewCycleState(), pgInfo, schedulingFunc)

			afterMetric, err := testutil.GetCounterMetricValue(metrics.WorkloadPreemptionAttempts.WithLabelValues(metricLabel))
			if err != nil {
				t.Fatalf("Failed to read WorkloadPreemptionAttempts metric: %v", err)
			}
			if diff := afterMetric - beforeMetric; diff != 1 {
				t.Errorf("Expected WorkloadPreemptionAttempts(%q) delta to be 1, got %v", metricLabel, diff)
			}

			if !mutableLister.startCalled {
				t.Errorf("Expected StartMutations to be called")
			}
			if mutableLister.endCalled != tt.wantEndCalled {
				t.Errorf("EndMutations called = %v, want %v", mutableLister.endCalled, tt.wantEndCalled)
			}
			if evaluator.called != tt.wantEvaluatorCalls {
				t.Errorf("podGroupEvaluator.Preempt called = %v, want %v", evaluator.called, tt.wantEvaluatorCalls)
			}

			if gotStatus.Code() != tt.expectedStatus.Code() || gotStatus.Message() != tt.expectedStatus.Message() {
				t.Errorf("Status mismatch: want (%v, %q), got (%v, %q)",
					tt.expectedStatus.Code(), tt.expectedStatus.Message(), gotStatus.Code(), gotStatus.Message())
			}

			if diff := cmp.Diff(tt.expectedResult, gotResult); diff != "" {
				t.Errorf("Result mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
