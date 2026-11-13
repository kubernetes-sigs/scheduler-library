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
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/preemption"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

type nilPreemptionManagerFramework struct {
	framework.Framework
}

type noopCacheWrapper struct {
	cache.Cache
}

func (nilPreemptionManagerFramework) PreemptionManager() fwk.PreemptionManager {
	return nil
}

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
	mayFitCalls     []sets.Set[preemption.PreemptionVictim]
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
	f.mayFitCalls = append(f.mayFitCalls, f.activeVictimSet.Clone())
	if f.minVictims > 0 {
		return f.activeVictimSet.Len() >= f.minVictims
	}
	return f.fit
}

func TestEvaluateWorkloadPreemption(t *testing.T) {
	ctx := t.Context()

	// Victims for 6-CPU node1 scenarios:
	v2CPUa := makeVictimOnNode("v2CPUa", "node1", "2")
	v2CPUb := makeVictimOnNode("v2CPUb", "node1", "2")
	v2CPUc := makeVictimOnNode("v2CPUc", "node1", "2")
	v4CPU := makeVictimOnNode("v4CPU", "node1", "4")
	vNode3 := makeVictimOnNode("vNode3", "node3", "4")

	// Victims for binary-search scenario (workload requests 6 CPU):
	// b0..b5 (1 CPU each) and b8..b9 (2 CPU each) occupy 10 CPU on node1 (10 CPU capacity).
	// b6 (4 CPU) and b7 (2 CPU) occupy 6 CPU on node2 (6 CPU capacity, listed before node1).
	b0 := makeVictimOnNode("b0", "node1", "1")
	b1 := makeVictimOnNode("b1", "node1", "1")
	b2 := makeVictimOnNode("b2", "node1", "1")
	b3 := makeVictimOnNode("b3", "node1", "1")
	b4 := makeVictimOnNode("b4", "node1", "1")
	b5 := makeVictimOnNode("b5", "node1", "1")
	b6 := makeVictimOnNode("b6", "node2", "4")
	b7 := makeVictimOnNode("b7", "node2", "2")
	b8 := makeVictimOnNode("b8", "node1", "2")
	b9 := makeVictimOnNode("b9", "node1", "2")
	tenSearchVictims := []preemption.PreemptionVictim{b0, b1, b2, b3, b4, b5, b6, b7, b8, b9}

	// Zero-pod victims (nil and empty pod slices):
	vNilPods := &testVictim{name: "vNilPods", pods: nil}
	vEmptyPodsA := &testVictim{name: "vEmptyPodsA", pods: []*v1.Pod{}}
	vEmptyPodsB := &testVictim{name: "vEmptyPodsB", pods: []*v1.Pod{}}

	tests := []struct {
		name                 string
		nodes                []*v1.Node
		workloadCPU          string
		workloadCPUs         []string
		potentialVictims     []preemption.PreemptionVictim
		filter               *fakeFilter
		nilPreemptionManager bool
		wantStatusCode       fwk.Code
		wantVictims          []preemption.PreemptionVictim
		wantMayFitCalls      []sets.Set[preemption.PreemptionVictim]
	}{
		{
			name:           "nil PotentialVictims returns Unschedulable",
			nodes:          []*v1.Node{makeNode("node1", "6")},
			workloadCPU:    "4",
			wantStatusCode: fwk.Unschedulable,
		},
		{
			name:             "nil victim returns Error status",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{nil},
			filter:           newFakeFilter(true),
			wantStatusCode:   fwk.Error,
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
			name: "exponential and binary search narrows prefix to 6 victims before reprieval, protecting higher-priority victims on node2",
			nodes: []*v1.Node{
				makeNode("node2", "6"),
				makeNode("node1", "10"),
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
			name: "PreemptionFilter (min 3 victims) combined with exponential/binary search and reprieval",
			nodes: []*v1.Node{
				makeNode("node1", "7"),
				makeNode("node2", "3"),
			},
			workloadCPU: "4",
			potentialVictims: []preemption.PreemptionVictim{
				b6, b7, b0, b1, b2, v4CPU,
			},
			filter:         newMinVictimsFilter(3),
			wantStatusCode: fwk.Success,
			// v4CPU is kept for node1; b2, b1, b0 are reprieved;
			// b6 and b7 are kept because MayFit() requires at least 3 victims.
			wantVictims: []preemption.PreemptionVictim{b6, b7, v4CPU},
		},
		{
			name: "CompareVictimSets ranks node with more-preferred preemption victim ahead of earlier-listed node",
			nodes: []*v1.Node{
				makeNode("node1", "2"),
				makeNode("node2", "2"),
				makeNode("node3", "4"),
			},
			workloadCPUs: []string{"4", "2"},
			potentialVictims: []preemption.PreemptionVictim{
				b7,     // index 0: 2 CPU on node2
				v2CPUa, // index 1: 2 CPU on node1
				vNode3, // index 2: 4 CPU on node3
			},
			wantStatusCode: fwk.Success,
			// At prefix size 3, all 3 nodes are free. The 4-CPU pod schedules on node3,
			// and CompareVictimSets ranks node2 (victim index 0) ahead of node1 (victim index 1)
			// for the 2-CPU pod, so v2CPUa on node1 is reprieved.
			wantVictims: []preemption.PreemptionVictim{b7, vNode3},
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
			potentialVictims: []preemption.PreemptionVictim{b0, b1, b2},
			filter:           newFakeFilter(true),
			wantStatusCode:   fwk.Unschedulable,
		},
		{
			name:             "victim pod on nonexistent node returns Error status during exponential search",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{makeVictimOnNode("v-missing-node", "missing-node", "2")},
			wantStatusCode:   fwk.Error,
		},
		{
			name:        "victim pod on nonexistent node returns Error status during trailing partial step",
			nodes:       []*v1.Node{makeNode("node1", "4")},
			workloadCPU: "6",
			potentialVictims: []preemption.PreemptionVictim{
				v2CPUa,
				v2CPUb,
				makeVictimOnNode("v-missing-node", "missing-node", "2"),
			},
			wantStatusCode: fwk.Error,
		},
		{
			name:                 "framework built without a PreemptionManager returns Error status",
			nodes:                []*v1.Node{makeNode("node1", "6")},
			workloadCPU:          "4",
			potentialVictims:     []preemption.PreemptionVictim{v4CPU},
			nilPreemptionManager: true,
			wantStatusCode:       fwk.Error,
		},
		{
			name:                 "framework built without a PreemptionManager returns Error status even with nil PotentialVictims",
			nodes:                []*v1.Node{makeNode("node1", "6")},
			workloadCPU:          "4",
			nilPreemptionManager: true,
			wantStatusCode:       fwk.Error,
		},
		{
			name:             "zero-pod victims satisfy PreemptionFilter when node has capacity",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA, vEmptyPodsB},
			filter:           newMinVictimsFilter(2),
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{vNilPods, vEmptyPodsA},
			wantMayFitCalls: []sets.Set[preemption.PreemptionVictim]{
				sets.New[preemption.PreemptionVictim](vNilPods),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods),
				sets.New[preemption.PreemptionVictim](vEmptyPodsA),
			},
		},
		{
			name:             "zero-pod victims combined with pod-bearing victim reprieves unneeded zero-pod victim and keeps needed one",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA, v4CPU},
			filter:           newMinVictimsFilter(2),
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{vNilPods, v4CPU},
			wantMayFitCalls: []sets.Set[preemption.PreemptionVictim]{
				sets.New[preemption.PreemptionVictim](vNilPods),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA, v4CPU),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA, v4CPU),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods, v4CPU),
				sets.New[preemption.PreemptionVictim](v4CPU),
			},
		},
		{
			name:             "binary search narrows prefix across zero-pod and pod-bearing victims",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA, vEmptyPodsB, v2CPUa},
			filter:           newMinVictimsFilter(3),
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{vNilPods, vEmptyPodsA, vEmptyPodsB},
			wantMayFitCalls: []sets.Set[preemption.PreemptionVictim]{
				sets.New[preemption.PreemptionVictim](vNilPods),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA, vEmptyPodsB, v2CPUa),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA, vEmptyPodsB),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA, vEmptyPodsB),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsB),
				sets.New[preemption.PreemptionVictim](vEmptyPodsA, vEmptyPodsB),
			},
		},
		{
			name:             "zero-pod victims without PreemptionFilter are reprieved when node has capacity",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA},
			wantStatusCode:   fwk.Success,
			wantVictims:      nil,
		},
		{
			name:             "zero-pod victims before pod-bearing victim without PreemptionFilter are reprieved",
			nodes:            []*v1.Node{makeNode("node1", "6")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA, v4CPU},
			wantStatusCode:   fwk.Success,
			wantVictims:      []preemption.PreemptionVictim{v4CPU},
		},
		{
			name:             "zero-pod victims insufficient when node lacks capacity returns Unschedulable",
			nodes:            []*v1.Node{makeNode("node1", "2")},
			workloadCPU:      "4",
			potentialVictims: []preemption.PreemptionVictim{vNilPods, vEmptyPodsA},
			filter:           newFakeFilter(true),
			wantStatusCode:   fwk.Unschedulable,
			wantMayFitCalls: []sets.Set[preemption.PreemptionVictim]{
				sets.New[preemption.PreemptionVictim](vNilPods),
				sets.New[preemption.PreemptionVictim](vNilPods, vEmptyPodsA),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeNames := sets.New[string]()
			for _, n := range tt.nodes {
				nodeNames.Insert(n.Name)
			}
			var existingPods []*v1.Pod
			for _, v := range tt.potentialVictims {
				if v == nil {
					continue
				}
				for _, p := range v.Pods() {
					if p != nil && nodeNames.Has(p.Spec.NodeName) {
						existingPods = append(existingPods, p.DeepCopy())
					}
				}
			}

			cpus := tt.workloadCPUs
			if len(cpus) == 0 {
				cpus = []string{tt.workloadCPU}
			}
			workloadPods := make([]*v1.Pod, len(cpus))
			for i, cpu := range cpus {
				workloadPods[i] = testutils.MakePod(fmt.Sprintf("w-pod-%d", i), "gang-pg", cpu)
			}
			pg := testutils.MakeGangPodGroup("gang-pg", "", int32(len(workloadPods)))

			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				existingPods,
				tt.nodes,
				[]*schedulingv1beta1.PodGroup{pg},
				nil,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}

			queuedPodGroupInfo, err := snapshot.BuildGroupHierarchyForTest(snap, workloadPods)
			if err != nil {
				t.Fatalf("BuildGroupHierarchyForTest failed: %v", err)
			}
			podGroupInfo := queuedPodGroupInfo.PodGroupInfo
			schedFramework, err := profileMap.FrameworkForPodGroup(podGroupInfo)
			if err != nil {
				t.Fatalf("FrameworkForPodGroup failed: %v", err)
			}
			if tt.nilPreemptionManager {
				schedFramework = nilPreemptionManagerFramework{Framework: schedFramework}
			}

			podAlg, err := scheduler.NewSchedulingAlgorithm(snap, noopCacheWrapper{})
			if err != nil {
				t.Fatalf("NewSchedulingAlgorithm failed: %v", err)
			}
			pgAlg := scheduler.NewPodGroupAlgorithm(snap, podAlg)
			cycleState := framework.NewCycleState()
			baseSchedFn := pgAlg.BuildPodGroupSchedulingFunc(schedFramework, cycleState, queuedPodGroupInfo)

			var opts preemption.WorkloadPreemptionOptions
			if tt.filter != nil {
				opts.PreemptionFilter = tt.filter
			}
			if tt.potentialVictims != nil {
				opts.PotentialVictims = slices.Values(tt.potentialVictims)
			}

			gotAssignments, gotVictims, gotStatus := preemption.EvaluateWorkloadPreemption(
				ctx, schedFramework, cycleState, podGroupInfo, baseSchedFn, opts,
			)
			if gotStatus.Code() != tt.wantStatusCode {
				t.Fatalf("EvaluateWorkloadPreemption() Status.Code() = %v, want %v (status: %v)", gotStatus.Code(), tt.wantStatusCode, gotStatus)
			}
			if !slices.Equal(gotVictims, tt.wantVictims) {
				t.Errorf("EvaluateWorkloadPreemption() victims = %v, want %v", gotVictims, tt.wantVictims)
			}
			if pm := schedFramework.PreemptionManager(); pm != nil {
				if victims, _ := pm.GenerateVictims(ctx, podGroupInfo); len(victims) != 0 {
					t.Errorf("GenerateVictims() after EvaluateWorkloadPreemption returned %d victims, want 0", len(victims))
				}
				if reprieve, err := pm.NewReprieveFilter(ctx, nil).ShouldAttemptReprieval(ctx, nil); err != nil || reprieve {
					t.Errorf("ShouldAttemptReprieval() after EvaluateWorkloadPreemption = %v, %v, want false, nil", reprieve, err)
				}
			}
			if tt.filter != nil {
				if tt.wantStatusCode != fwk.Error && len(tt.potentialVictims) > 0 {
					if len(tt.filter.mayFitCalls) == 0 {
						t.Errorf("expected PreemptionFilter.MayFit to be called, got 0 calls")
					} else if tt.filter.mayFitCalls[0].Len() == 0 {
						t.Errorf("expected PreemptionFilter.PreemptVictim to be called before MayFit, got empty active set")
					}
				}
				if tt.wantMayFitCalls != nil {
					if diff := cmp.Diff(tt.wantMayFitCalls, tt.filter.mayFitCalls); diff != "" {
						t.Errorf("filter.mayFitCalls mismatch (-want +got):\n%s", diff)
					}
				}
			}
			if !gotStatus.IsSuccess() {
				if gotAssignments != nil {
					t.Errorf("EvaluateWorkloadPreemption() assignments = %v, want nil", gotAssignments)
				}
				return
			}
			if tt.filter != nil && !tt.filter.activeVictimSet.Equal(sets.New(tt.wantVictims...)) {
				t.Errorf("filter.activeVictimSet = %v, want %v", tt.filter.activeVictimSet.UnsortedList(), tt.wantVictims)
			}
			if gotAssignments == nil || len(gotAssignments.ProposedAssignments) != len(workloadPods) {
				t.Errorf("EvaluateWorkloadPreemption() assignments = %v, want %d proposed assignments", gotAssignments, len(workloadPods))
			}
		})
	}
}

// TestEvaluateWorkloadPreemptionRepeated checks that a call leaves nothing behind in the
// PreemptionManager that a later call on the same framework could pick up.
func TestEvaluateWorkloadPreemptionRepeated(t *testing.T) {
	ctx := t.Context()

	v2CPUa := makeVictimOnNode("v2CPUa", "node1", "2")
	v2CPUb := makeVictimOnNode("v2CPUb", "node1", "2")
	v4CPU := makeVictimOnNode("v4CPU", "node1", "4")

	profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
		ctx,
		t,
		[]*v1.Pod{v2CPUa.pods[0].DeepCopy(), v2CPUb.pods[0].DeepCopy(), v4CPU.pods[0].DeepCopy()},
		[]*v1.Node{makeNode("node1", "8")},
		[]*schedulingv1beta1.PodGroup{testutils.MakeGangPodGroup("gang-pg", "", 1)},
		nil,
	)
	if err != nil {
		t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
	}
	queuedPodGroupInfo, err := snapshot.BuildGroupHierarchyForTest(snap, []*v1.Pod{testutils.MakePod("w-pod", "gang-pg", "4")})
	if err != nil {
		t.Fatalf("BuildGroupHierarchyForTest failed: %v", err)
	}
	podGroupInfo := queuedPodGroupInfo.PodGroupInfo
	schedFramework, err := profileMap.FrameworkForPodGroup(podGroupInfo)
	if err != nil {
		t.Fatalf("FrameworkForPodGroup failed: %v", err)
	}
	podAlg, err := scheduler.NewSchedulingAlgorithm(snap, noopCacheWrapper{})
	if err != nil {
		t.Fatalf("NewSchedulingAlgorithm failed: %v", err)
	}
	pgAlg := scheduler.NewPodGroupAlgorithm(snap, podAlg)

	calls := []struct {
		opts        preemption.WorkloadPreemptionOptions
		wantVictims []preemption.PreemptionVictim
	}{
		{
			opts: preemption.WorkloadPreemptionOptions{
				PotentialVictims: slices.Values([]preemption.PreemptionVictim{v2CPUa, v2CPUb}),
				PreemptionFilter: newMinVictimsFilter(2),
			},
			wantVictims: []preemption.PreemptionVictim{v2CPUa, v2CPUb},
		},
		{
			opts: preemption.WorkloadPreemptionOptions{
				PotentialVictims: slices.Values([]preemption.PreemptionVictim{v4CPU}),
			},
			wantVictims: []preemption.PreemptionVictim{v4CPU},
		},
	}

	for i, call := range calls {
		cycleState := framework.NewCycleState()
		baseSchedFn := pgAlg.BuildPodGroupSchedulingFunc(schedFramework, cycleState, queuedPodGroupInfo)
		_, gotVictims, gotStatus := preemption.EvaluateWorkloadPreemption(
			ctx, schedFramework, cycleState, podGroupInfo, baseSchedFn, call.opts,
		)
		if !gotStatus.IsSuccess() {
			t.Fatalf("call %d: EvaluateWorkloadPreemption() status = %v, want success", i+1, gotStatus)
		}
		if !slices.Equal(gotVictims, call.wantVictims) {
			t.Errorf("call %d: EvaluateWorkloadPreemption() victims = %v, want %v", i+1, gotVictims, call.wantVictims)
		}
	}
}
