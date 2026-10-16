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

package preemption_test

import (
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/preemption"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

type testVictim struct {
	name string
	pods []*v1.Pod
}

func (v *testVictim) Pods() []*v1.Pod {
	return v.pods
}

func (v *testVictim) String() string {
	return v.name
}

func makeNode(name, cpu string) *v1.Node {
	return st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    cpu,
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()
}

func makeVictimOnNode(name, nodeName, cpu string) *testVictim {
	pod := testutils.MakePod(name+"-pod", "", cpu)
	pod.Spec.NodeName = nodeName
	return &testVictim{
		name: name,
		pods: []*v1.Pod{pod},
	}
}

type fakeFilter struct {
	fit             bool
	minVictims      int
	activeVictimSet sets.Set[preemption.PreemptionVictim]
}

func newFakeFilter(fit bool) *fakeFilter {
	return &fakeFilter{
		fit:             fit,
		activeVictimSet: sets.New[preemption.PreemptionVictim](),
	}
}

func newMinVictimsFilter(minVictims int) *fakeFilter {
	return &fakeFilter{
		minVictims:      minVictims,
		activeVictimSet: sets.New[preemption.PreemptionVictim](),
	}
}

func (f *fakeFilter) PreemptVictim(v preemption.PreemptionVictim) {
	f.activeVictimSet.Insert(v)
}

func (f *fakeFilter) UnpreemptVictim(v preemption.PreemptionVictim) {
	f.activeVictimSet.Delete(v)
}

func (f *fakeFilter) MayFit() bool {
	if f.minVictims > 0 {
		return f.activeVictimSet.Len() >= f.minVictims
	}
	return f.fit
}

func TestEvaluateWorkloadPreemption(t *testing.T) {
	ctx := t.Context()

	gangPG := testutils.MakeGangPodGroup("gang-pg", "", 1)

	// Victims for 6-CPU node1 scenarios:
	v2CPUa := makeVictimOnNode("v2CPUa", "node1", "2")
	v2CPUb := makeVictimOnNode("v2CPUb", "node1", "2")
	v2CPUc := makeVictimOnNode("v2CPUc", "node1", "2")
	v4CPU := makeVictimOnNode("v4CPU", "node1", "4")

	// Victims for 10-CPU node1 binary-search scenario (workload requests 6 CPU):
	b0 := makeVictimOnNode("b0", "node1", "1")
	b1 := makeVictimOnNode("b1", "node1", "1")
	b2 := makeVictimOnNode("b2", "node1", "1")
	b3 := makeVictimOnNode("b3", "node1", "1")
	b4 := makeVictimOnNode("b4", "node1", "1")
	b5 := makeVictimOnNode("b5", "node1", "1")
	b6 := makeVictimOnNode("b6", "node1", "4")
	b7 := makeVictimOnNode("b7", "node2", "1")
	b8 := makeVictimOnNode("b8", "node2", "1")
	b9 := makeVictimOnNode("b9", "node2", "1")
	tenSearchVictims := []preemption.PreemptionVictim{b0, b1, b2, b3, b4, b5, b6, b7, b8, b9}

	tests := []struct {
		name             string
		nodes            []*v1.Node
		workloadCPU      string
		potentialVictims []preemption.PreemptionVictim
		filter           *fakeFilter
		wantStatusCode   fwk.Code
		wantVictims      []preemption.PreemptionVictim
	}{
		{
			name:           "nil PotentialVictims returns Unschedulable",
			nodes:          []*v1.Node{makeNode("node1", "6")},
			workloadCPU:    "4",
			wantStatusCode: fwk.Unschedulable,
		},
		{
			name:        "nil and empty-pod victims are skipped",
			nodes:       []*v1.Node{makeNode("node1", "6")},
			workloadCPU: "4",
			potentialVictims: []preemption.PreemptionVictim{
				nil,
				&testVictim{name: "nil-pods", pods: nil},
				&testVictim{name: "empty-pods", pods: []*v1.Pod{}},
			},
			filter:         newFakeFilter(true),
			wantStatusCode: fwk.Unschedulable,
		},
		{
			name:        "victim with nil pod returns Error status",
			nodes:       []*v1.Node{makeNode("node1", "6")},
			workloadCPU: "4",
			potentialVictims: []preemption.PreemptionVictim{
				&testVictim{name: "nil-pod", pods: []*v1.Pod{nil}},
			},
			wantStatusCode: fwk.Error,
		},
		{
			name:             "prefix search stops at first fitting prefix",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{v2CPUa, v2CPUb, v2CPUc},
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{v2CPUa, v2CPUb},
		},
		{
			name:             "PreemptionFilter skips probes until MayFit is true",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "2",
			potentialVictims: []preemption.PreemptionVictim{v2CPUa, v2CPUb, v2CPUc},
			filter:           newMinVictimsFilter(2),
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{v2CPUa, v2CPUb},
		},
		{
			name:             "reprieval spares unneeded lower-priority victim when second victim suffices",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{v2CPUa, v4CPU},
			filter:           newFakeFilter(true),
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{v4CPU},
		},
		{
			name: "exponential and binary search narrows prefix to k*=6 before reprieval, protecting higher-priority victim b6",
			nodes: []*v1.Node{
				makeNode("node1", "10"),
				makeNode("node2", "3"),
			},
			workloadCPU:      "6",
			potentialVictims: tenSearchVictims,
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{b0, b1, b2, b3, b4, b5},
		},
		{
			name:             "trailing partial exponential step when iterator ends mid-step (N=6 between 4 and 8)",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "6",
			potentialVictims: []preemption.PreemptionVictim{b0, b1, b2, b3, b4, b5},
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{b0, b1, b2, b3, b4, b5},
		},
		{
			name: "quota fast-forward (min 3 victims) combined with exponential/binary search and reprieval",
			nodes: []*v1.Node{
				makeNode("node1", "6"),
				makeNode("node2", "3"),
			},
			workloadCPU: "4",
			potentialVictims: []preemption.PreemptionVictim{
				b7, b8, b9, b0, b1, v4CPU,
			},
			filter:         newMinVictimsFilter(3),
			wantStatusCode: fwk.Success,
			// v4CPU is kept for node1; b1, b0, b9 are reprieved;
			// b7 and b8 are kept because MayFit() requires at least 3 victims.
			wantVictims: []preemption.PreemptionVictim{b7, b8, v4CPU},
		},
		{
			name:             "PreemptionFilter MayFit never satisfied returns Unschedulable",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{v4CPU, v2CPUa},
			filter:           newFakeFilter(false),
			wantStatusCode:   fwk.Unschedulable,
		},
		{
			name:             "all potential victims insufficient returns Unschedulable",
			nodes:            []*v1.Node{makeNode("node1", "4")},
			workloadCPU:      "6",
			potentialVictims: []preemption.PreemptionVictim{v2CPUa, v2CPUb},
			filter:           newFakeFilter(true),
			wantStatusCode:   fwk.Unschedulable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var existingPods []*v1.Pod
			for _, v := range tt.potentialVictims {
				if v == nil {
					continue
				}
				for _, p := range v.Pods() {
					if p != nil {
						existingPods = append(existingPods, p.DeepCopy())
					}
				}
			}

			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				existingPods,
				tt.nodes,
				[]*schedulingv1beta1.PodGroup{gangPG},
				nil,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}

			workloadPod := testutils.MakePod("w-pod", "gang-pg", tt.workloadCPU)
			podGroupInfo, err := snapshot.BuildGroupHierarchyForTest(snap, []*v1.Pod{workloadPod})
			if err != nil {
				t.Fatalf("BuildGroupHierarchyForTest failed: %v", err)
			}
			schedFramework, err := profileMap.FrameworkForPodGroup(podGroupInfo)
			if err != nil {
				t.Fatalf("FrameworkForPodGroup failed: %v", err)
			}

			sched := upstreamsync.NewScheduler(snap, 0, 0, 1, nil)
			baseSchedFn := sched.BuildPodGroupSchedulingFunc(schedFramework, podGroupInfo)

			var opts preemption.WorkloadPreemptionOptions
			if tt.filter != nil {
				opts.PreemptionFilter = tt.filter
			}
			if tt.potentialVictims != nil {
				opts.PotentialVictims = slices.Values(tt.potentialVictims)
			}

			gotAssignments, gotVictims, gotStatus := preemption.EvaluateWorkloadPreemption(
				ctx, schedFramework, framework.NewCycleState(), podGroupInfo, baseSchedFn, opts,
			)
			if gotStatus.Code() != tt.wantStatusCode {
				t.Fatalf("EvaluateWorkloadPreemption() Status.Code() = %v, want %v (status: %v)", gotStatus.Code(), tt.wantStatusCode, gotStatus)
			}
			if !slices.Equal(gotVictims, tt.wantVictims) {
				t.Errorf("EvaluateWorkloadPreemption() victims = %v, want %v", gotVictims, tt.wantVictims)
			}
			if _, ok := schedFramework.PreemptionManager().(*preemption.NoopPreemptionManager); !ok {
				t.Errorf("expected PreemptionManager to be reset to NoopPreemptionManager, got %T", schedFramework.PreemptionManager())
			}
			if !gotStatus.IsSuccess() {
				if gotAssignments != nil {
					t.Errorf("EvaluateWorkloadPreemption() assignments = %v, want nil", gotAssignments)
				}
				return
			}
			if gotAssignments == nil || len(gotAssignments.ProposedAssignments) != 1 {
				t.Errorf("EvaluateWorkloadPreemption() assignments = %v, want 1 proposed assignment", gotAssignments)
			}
		})
	}
}
