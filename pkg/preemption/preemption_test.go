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
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	upstreampreemption "sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

type testVictim struct {
	pods []*v1.Pod
}

func (v *testVictim) Pods() []*v1.Pod {
	return v.pods
}

type fakeFilter struct {
	fit             bool
	minVictims      int
	preemptCalls    int
	unpreemptCalls  int
	activeVictimSet map[PreemptionVictim]bool
}

func newFakeFilter(fit bool) *fakeFilter {
	return &fakeFilter{
		fit:             fit,
		activeVictimSet: make(map[PreemptionVictim]bool),
	}
}

func (f *fakeFilter) PreemptVictim(v PreemptionVictim) {
	f.preemptCalls++
	f.activeVictimSet[v] = true
}

func (f *fakeFilter) UnpreemptVictim(v PreemptionVictim) {
	f.unpreemptCalls++
	delete(f.activeVictimSet, v)
}

func (f *fakeFilter) MayFit() bool {
	if f.minVictims > 0 {
		return len(f.activeVictimSet) >= f.minVictims
	}
	return f.fit
}

type mockFramework struct {
	framework.Framework
	postFilterCalls int
	postFilterFn    func(ctx context.Context, state *framework.CycleState, podGroupInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status)
}

func (m *mockFramework) RunPodGroupPostFilterPlugins(
	ctx context.Context,
	state *framework.CycleState,
	podGroupInfo fwk.PodGroupInfo,
	pgSchedulingFunc fwk.PodGroupSchedulingFunc,
) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
	m.postFilterCalls++
	if m.postFilterFn != nil {
		return m.postFilterFn(ctx, state, podGroupInfo, pgSchedulingFunc)
	}
	return nil, fwk.NewStatus(fwk.Unschedulable)
}

func TestEvaluateWorkloadPreemption(t *testing.T) {
	ctx := context.Background()
	pod1 := st.MakePod().Namespace("default").Name("p1").Node("n1").Obj()
	pod2 := st.MakePod().Namespace("default").Name("p2").Node("n1").Obj()
	pod3 := st.MakePod().Namespace("default").Name("p3").Node("n1").Obj()

	v1Victim := &testVictim{pods: []*v1.Pod{pod1}}
	v2Victim := &testVictim{pods: []*v1.Pod{pod2}}
	v3Victim := &testVictim{pods: []*v1.Pod{pod3}}

	assignments1 := &fwk.PodGroupAssignments{}
	assignments2 := &fwk.PodGroupAssignments{}
	expectedErr := errors.New("postfilter plugin error")

	tests := []struct {
		name                string
		nilPotentialVictims bool
		potentialVictims    []PreemptionVictim
		filter              *fakeFilter
		baseSchedFn         fwk.PodGroupSchedulingFunc
		postFilterFn        func(ctx context.Context, pf *upstreampreemption.PreemptionFramework, pgInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status)
		wantStatusCode      fwk.Code
		wantErr             error
		wantAssignments     *fwk.PodGroupAssignments
		wantVictims         []PreemptionVictim
		wantPostFilterCalls int
		wantYieldedCount    int
		wantPreemptCalls    int
		wantActiveVictims   map[PreemptionVictim]bool
	}{
		{
			name:                "nil PotentialVictims returns Unschedulable",
			nilPotentialVictims: true,
			wantStatusCode:      fwk.Unschedulable,
			wantPostFilterCalls: 0,
		},
		{
			name: "nil and empty-pod victims return Error status",
			potentialVictims: []PreemptionVictim{
				nil,
				&testVictim{pods: nil},
				&testVictim{pods: []*v1.Pod{}},
			},
			filter:              newFakeFilter(true),
			wantStatusCode:      fwk.Error,
			wantPostFilterCalls: 0,
			wantYieldedCount:    1,
			wantPreemptCalls:    0,
		},
		{
			name: "victim with nil pod returns Error status",
			potentialVictims: []PreemptionVictim{
				&testVictim{pods: []*v1.Pod{nil}},
			},
			wantStatusCode:      fwk.Error,
			wantPostFilterCalls: 0,
			wantYieldedCount:    1,
		},
		{
			name:             "prefix search stops at first fitting prefix and resets PreemptionManager",
			potentialVictims: []PreemptionVictim{v1Victim, v2Victim, v3Victim},
			baseSchedFn: func(_ context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
				return assignments1, fwk.NewStatus(fwk.Success)
			},
			postFilterFn: func(ctx context.Context, pf *upstreampreemption.PreemptionFramework, pgInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
				pm := pf.PreemptionManager()
				victims, status := pm.GenerateVictims(ctx, pgInfo)
				if !status.IsSuccess() {
					return nil, status
				}
				if len(victims) < 2 {
					return nil, fwk.NewStatus(fwk.Unschedulable)
				}
				if _, schedStatus := pgSchedulingFunc(ctx); !schedStatus.IsSuccess() {
					return nil, schedStatus
				}
				return &fwk.PodGroupPostFilterResult{}, fwk.NewStatus(fwk.Success)
			},
			wantStatusCode:      fwk.Success,
			wantAssignments:     assignments1,
			wantVictims:         []PreemptionVictim{v1Victim, v2Victim},
			wantPostFilterCalls: 2,
			wantYieldedCount:    2,
		},
		{
			name:             "PreemptionFilter skips RunPodGroupPostFilterPlugins until MayFit is true",
			potentialVictims: []PreemptionVictim{v1Victim, v2Victim, v3Victim},
			filter: &fakeFilter{
				fit:             false,
				minVictims:      2,
				activeVictimSet: make(map[PreemptionVictim]bool),
			},
			baseSchedFn: func(_ context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
				return assignments1, fwk.NewStatus(fwk.Success)
			},
			postFilterFn: func(ctx context.Context, pf *upstreampreemption.PreemptionFramework, pgInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
				pm := pf.PreemptionManager()
				victims, _ := pm.GenerateVictims(ctx, pgInfo)
				if len(victims) != 2 {
					t.Errorf("expected 2 accumulated victims when PostFilter runs, got %d", len(victims))
				}
				if _, schedStatus := pgSchedulingFunc(ctx); !schedStatus.IsSuccess() {
					return nil, schedStatus
				}
				return &fwk.PodGroupPostFilterResult{}, fwk.NewStatus(fwk.Success)
			},
			wantStatusCode:      fwk.Success,
			wantAssignments:     assignments1,
			wantVictims:         []PreemptionVictim{v1Victim, v2Victim},
			wantPostFilterCalls: 1,
			wantYieldedCount:    2,
			wantPreemptCalls:    2,
		},
		{
			name:             "reprieval filters out reprieved victims and preserves last successful assignments",
			potentialVictims: []PreemptionVictim{v1Victim, v2Victim},
			filter:           newFakeFilter(true),
			baseSchedFn: func() fwk.PodGroupSchedulingFunc {
				schedCalls := 0
				return func(_ context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
					schedCalls++
					switch schedCalls {
					case 1:
						return assignments1, fwk.NewStatus(fwk.Success)
					case 2:
						return nil, fwk.NewStatus(fwk.Unschedulable)
					case 3:
						return assignments2, fwk.NewStatus(fwk.Success)
					default:
						return nil, fwk.NewStatus(fwk.Unschedulable)
					}
				}
			}(),
			postFilterFn: func(ctx context.Context, pf *upstreampreemption.PreemptionFramework, pgInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
				pm := pf.PreemptionManager()
				victims, _ := pm.GenerateVictims(ctx, pgInfo)
				if len(victims) < 2 {
					return nil, fwk.NewStatus(fwk.Unschedulable)
				}
				// Initial fit check with all victims removed succeeds (assignments1).
				if _, schedStatus := pgSchedulingFunc(ctx); !schedStatus.IsSuccess() {
					return nil, schedStatus
				}
				rf := pm.NewReprieveFilter(ctx, victims)
				// Reprieving victims[1] (v2Victim) fails scheduling check (returns nil, Unschedulable).
				if ok, err := rf.ShouldAttemptReprieval(ctx, victims[1]); err != nil || !ok {
					t.Fatalf("expected ShouldAttemptReprieval for victims[1] to succeed, got %v, %v", ok, err)
				}
				if _, schedStatus := pgSchedulingFunc(ctx); schedStatus.IsSuccess() {
					t.Fatalf("expected second scheduling call to fail")
				}
				// Reprieving victims[0] (v1Victim) succeeds scheduling check (assignments2).
				if ok, err := rf.ShouldAttemptReprieval(ctx, victims[0]); err != nil || !ok {
					t.Fatalf("expected ShouldAttemptReprieval for victims[0] to succeed, got %v, %v", ok, err)
				}
				if _, schedStatus := pgSchedulingFunc(ctx); !schedStatus.IsSuccess() {
					t.Fatalf("expected third scheduling call to succeed, got %v", schedStatus)
				}
				if err := rf.OnVictimReprieved(ctx, victims[0]); err != nil {
					t.Fatalf("unexpected error in OnVictimReprieved: %v", err)
				}

				return &fwk.PodGroupPostFilterResult{}, fwk.NewStatus(fwk.Success)
			},
			wantStatusCode:      fwk.Success,
			wantAssignments:     assignments2,
			wantVictims:         []PreemptionVictim{v2Victim},
			wantPostFilterCalls: 2,
			wantYieldedCount:    2,
			wantPreemptCalls:    4,
			wantActiveVictims:   map[PreemptionVictim]bool{v2Victim: true},
		},
		{
			name:             "RunPodGroupPostFilterPlugins error aborts immediately",
			potentialVictims: []PreemptionVictim{v1Victim, v2Victim},
			postFilterFn: func(_ context.Context, _ *upstreampreemption.PreemptionFramework, _ fwk.PodGroupInfo, _ fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
				return nil, fwk.AsStatus(expectedErr)
			},
			wantStatusCode:      fwk.Error,
			wantErr:             expectedErr,
			wantPostFilterCalls: 1,
			wantYieldedCount:    1,
		},
		{
			name:             "all potential victims insufficient returns Unschedulable",
			potentialVictims: []PreemptionVictim{v1Victim, v2Victim},
			postFilterFn: func(_ context.Context, _ *upstreampreemption.PreemptionFramework, _ fwk.PodGroupInfo, _ fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
				return nil, fwk.NewStatus(fwk.Unschedulable)
			},
			wantStatusCode:      fwk.Unschedulable,
			wantPostFilterCalls: 2,
			wantYieldedCount:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pf *upstreampreemption.PreemptionFramework
			mf := &mockFramework{
				postFilterFn: func(ctx context.Context, _ *framework.CycleState, pgInfo fwk.PodGroupInfo, pgSchedulingFunc fwk.PodGroupSchedulingFunc) (*fwk.PodGroupPostFilterResult, *fwk.Status) {
					if tt.postFilterFn != nil {
						return tt.postFilterFn(ctx, pf, pgInfo, pgSchedulingFunc)
					}
					return nil, fwk.NewStatus(fwk.Unschedulable)
				},
			}
			pf = upstreampreemption.NewPreemptionFramework(mf, &NoopPreemptionManager{})

			yieldedCount := 0
			var opts WorkloadPreemptionOptions
			if tt.filter != nil {
				opts.PreemptionFilter = tt.filter
			}
			if !tt.nilPotentialVictims {
				opts.PotentialVictims = func(yield func(PreemptionVictim) bool) {
					for _, v := range tt.potentialVictims {
						yieldedCount++
						if !yield(v) {
							return
						}
					}
				}
			}

			gotAssignments, gotVictims, gotStatus := EvaluateWorkloadPreemption(
				ctx, pf, framework.NewCycleState(), nil, tt.baseSchedFn, opts,
			)
			if gotStatus.Code() != tt.wantStatusCode {
				t.Fatalf("EvaluateWorkloadPreemption() Status.Code() = %v, want %v (status: %v)", gotStatus.Code(), tt.wantStatusCode, gotStatus)
			}
			if tt.wantErr != nil && !errors.Is(gotStatus.AsError(), tt.wantErr) {
				t.Errorf("EvaluateWorkloadPreemption() error = %v, want %v", gotStatus.AsError(), tt.wantErr)
			}
			if gotAssignments != tt.wantAssignments {
				t.Errorf("EvaluateWorkloadPreemption() assignments = %v, want %v", gotAssignments, tt.wantAssignments)
			}
			if !slices.Equal(gotVictims, tt.wantVictims) {
				t.Errorf("EvaluateWorkloadPreemption() victims = %v, want %v", gotVictims, tt.wantVictims)
			}
			if mf.postFilterCalls != tt.wantPostFilterCalls {
				t.Errorf("RunPodGroupPostFilterPlugins calls = %d, want %d", mf.postFilterCalls, tt.wantPostFilterCalls)
			}
			if yieldedCount != tt.wantYieldedCount {
				t.Errorf("yielded victims = %d, want %d", yieldedCount, tt.wantYieldedCount)
			}
			if tt.filter != nil && tt.filter.preemptCalls != tt.wantPreemptCalls {
				t.Errorf("filter.preemptCalls = %d, want %d", tt.filter.preemptCalls, tt.wantPreemptCalls)
			}
			if tt.wantActiveVictims != nil {
				for v, wantActive := range tt.wantActiveVictims {
					if tt.filter.activeVictimSet[v] != wantActive {
						t.Errorf("filter.activeVictimSet[%v] = %v, want %v", v, tt.filter.activeVictimSet[v], wantActive)
					}
				}
				if len(tt.filter.activeVictimSet) != len(tt.wantActiveVictims) {
					t.Errorf("filter.activeVictimSet = %v, want %v", tt.filter.activeVictimSet, tt.wantActiveVictims)
				}
			}
			if _, ok := pf.PreemptionManager().(*NoopPreemptionManager); !ok {
				t.Errorf("expected PreemptionManager to be reset to NoopPreemptionManager, got %T", pf.PreemptionManager())
			}
		})
	}
}
