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

package upstreamsync_test

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

func TestRunRootSchedulingAlgorithm(t *testing.T) {
	ctx := context.Background()

	node4CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPodGroup := testutils.MakeGangPodGroup("gang-pg", "", 2)
	gangPodGroupMin3 := testutils.MakeGangPodGroup("gang-pg-min3", "", 3)
	gangPod1 := testutils.MakePod("p1", "gang-pg", "3")
	gangPod2 := testutils.MakePod("p2", "gang-pg", "3")
	gangPod3 := testutils.MakePod("p3", "gang-pg", "3")
	gangMin3Pod1 := testutils.MakePod("p1", "gang-pg-min3", "3")
	gangMin3Pod2 := testutils.MakePod("p2", "gang-pg-min3", "3")
	gangMin3Pod3 := testutils.MakePod("p3", "gang-pg-min3", "3")

	basicPodGroup := testutils.MakeBasicPodGroup("basic-pg", "")
	basicPod1 := testutils.MakePod("p1", "basic-pg", "3")
	basicPod2 := testutils.MakePod("p2", "basic-pg", "3")

	compositePodGroup1 := testutils.MakeGangCompositePodGroup("gang-cpg1", "", 2)
	compositePodGroupMin1 := testutils.MakeGangCompositePodGroup("gang-cpg-min1", "", 1)
	compositePodGroupMin3 := testutils.MakeGangCompositePodGroup("gang-cpg-min3", "", 3)
	compositePodGroup1leaf1 := testutils.MakeGangPodGroup("cpg1-leaf1", "gang-cpg1", 1)
	compositePodGroup1leaf2 := testutils.MakeGangPodGroup("cpg1-leaf2", "gang-cpg1", 1)
	compositePodGroupMin1leaf1 := testutils.MakeGangPodGroup("cpg-min1-leaf1", "gang-cpg-min1", 1)
	compositePodGroupMin1leaf2 := testutils.MakeGangPodGroup("cpg-min1-leaf2", "gang-cpg-min1", 1)
	compositePodGroupMin3leaf1 := testutils.MakeGangPodGroup("cpg-min3-leaf1", "gang-cpg-min3", 1)
	compositePodGroupMin3leaf2 := testutils.MakeGangPodGroup("cpg-min3-leaf2", "gang-cpg-min3", 1)
	compositePodGroupMin3leaf3 := testutils.MakeGangPodGroup("cpg-min3-leaf3", "gang-cpg-min3", 1)
	compositePodGroup1leaf1Pod2CPU := testutils.MakePod("p1", "cpg1-leaf1", "2")
	compositePodGroup1leaf2Pod2CPU := testutils.MakePod("p2", "cpg1-leaf2", "2")
	compositePodGroup1leaf1Pod3CPU := testutils.MakePod("p1", "cpg1-leaf1", "3")
	compositePodGroup1leaf2Pod3CPU := testutils.MakePod("p2", "cpg1-leaf2", "3")
	compositePodGroupMin1leaf1Pod3CPU := testutils.MakePod("p1", "cpg-min1-leaf1", "3")
	compositePodGroupMin1leaf2Pod3CPU := testutils.MakePod("p2", "cpg-min1-leaf2", "3")
	compositePodGroupMin3leaf1Pod2CPU := testutils.MakePod("p1", "cpg-min3-leaf1", "2")
	compositePodGroupMin3leaf2Pod2CPU := testutils.MakePod("p2", "cpg-min3-leaf2", "2")
	compositePodGroupMin3leaf1Pod3CPU := testutils.MakePod("p1", "cpg-min3-leaf1", "3")
	compositePodGroupMin3leaf2Pod3CPU := testutils.MakePod("p2", "cpg-min3-leaf2", "3")
	compositePodGroupMin3leaf3Pod3CPU := testutils.MakePod("p3", "cpg-min3-leaf3", "3")

	basicCompositePodGroup := testutils.MakeBasicCompositePodGroup("root-basic", "")
	basicLeaf1 := testutils.MakeGangPodGroup("leaf-b1", "root-basic", 1)
	basicLeaf2 := testutils.MakeGangPodGroup("leaf-b2", "root-basic", 1)
	basicLeaf1Pod := testutils.MakePod("p1", "leaf-b1", "2")
	basicLeaf2Pod := testutils.MakePod("p2", "leaf-b2", "2")

	rootCompositePodGroup := testutils.MakeGangCompositePodGroup("root-cpg", "", 2)
	compositePodGroup2 := testutils.MakeGangCompositePodGroup("gang-cpg2", "root-cpg", 1)
	rootCompositePodGroupleaf := testutils.MakeGangPodGroup("root-cpg-leaf1", "root-cpg", 1)
	compositePodGroup2leaf := testutils.MakeGangPodGroup("cpg2-leaf1", "gang-cpg2", 1)
	rootCompositePodGroupleafPod := testutils.MakePod("p1", "root-cpg-leaf1", "2")
	compositePodGroup2leafPod := testutils.MakePod("p2", "cpg2-leaf1", "2")
	rootCompositePodGroupleafPod3CPU := testutils.MakePod("p1", "root-cpg-leaf1", "3")
	compositePodGroup2leafPod3CPU := testutils.MakePod("p2", "cpg2-leaf1", "3")

	tests := []struct {
		name                    string
		nodes                   []*v1.Node
		podGroups               []*schedulingv1beta1.PodGroup
		compositePodGroups      []*schedulingv1alpha3.CompositePodGroup
		pods                    []*v1.Pod
		wantKey                 fwk.EntityKey
		wantSuccess             bool
		wantPodResults          int
		expectedNodePods        map[string]sets.Set[string]
		expectedUnscheduledPods sets.Set[string]
		expectedSkippedKeys     []fwk.EntityKey
	}{
		{
			name:             "Single podgroup with gang policy",
			nodes:            []*v1.Node{node8CPU},
			podGroups:        []*schedulingv1beta1.PodGroup{gangPodGroup},
			pods:             []*v1.Pod{gangPod1, gangPod2},
			wantKey:          fwk.PodGroupKey("default", "gang-pg"),
			wantSuccess:      true,
			wantPodResults:   2,
			expectedNodePods: map[string]sets.Set[string]{"node1": sets.New("p1", "p2")},
		},
		{
			name:                    "Single podgroup with gang policy succeeds when minCount subset fits",
			nodes:                   []*v1.Node{node8CPU},
			podGroups:               []*schedulingv1beta1.PodGroup{gangPodGroup},
			pods:                    []*v1.Pod{gangPod1, gangPod2, gangPod3},
			wantKey:                 fwk.PodGroupKey("default", "gang-pg"),
			wantSuccess:             true,
			wantPodResults:          3,
			expectedNodePods:        map[string]sets.Set[string]{"node1": sets.New("p1", "p2")},
			expectedUnscheduledPods: sets.New("p3"),
		},
		{
			name:             "Single podgroup with gang policy fails early via PlacementFeasible when fewer pods than minCount",
			nodes:            []*v1.Node{node8CPU},
			podGroups:        []*schedulingv1beta1.PodGroup{gangPodGroupMin3},
			pods:             []*v1.Pod{gangMin3Pod1, gangMin3Pod2},
			wantKey:          fwk.PodGroupKey("default", "gang-pg-min3"),
			wantSuccess:      false,
			wantPodResults:   0,
			expectedNodePods: map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name:                    "Single podgroup with gang policy terminates loop early when minCount can no longer be met",
			nodes:                   []*v1.Node{node4CPU},
			podGroups:               []*schedulingv1beta1.PodGroup{gangPodGroupMin3},
			pods:                    []*v1.Pod{gangMin3Pod1, gangMin3Pod2, gangMin3Pod3},
			wantKey:                 fwk.PodGroupKey("default", "gang-pg-min3"),
			wantSuccess:             false,
			wantPodResults:          2,
			expectedNodePods:        map[string]sets.Set[string]{"node1": sets.New[string]()},
			expectedUnscheduledPods: sets.New("p2"),
		},
		{
			name:                    "Single podgroup scheduling failure due to insufficient capacity",
			nodes:                   []*v1.Node{node4CPU},
			podGroups:               []*schedulingv1beta1.PodGroup{gangPodGroup},
			pods:                    []*v1.Pod{gangPod1, gangPod2},
			wantKey:                 fwk.PodGroupKey("default", "gang-pg"),
			wantSuccess:             false,
			wantPodResults:          2,
			expectedNodePods:        map[string]sets.Set[string]{"node1": sets.New[string]()},
			expectedUnscheduledPods: sets.New("p2"),
		},
		{
			name:             "Single podgroup with basic policy",
			nodes:            []*v1.Node{node4CPU},
			podGroups:        []*schedulingv1beta1.PodGroup{basicPodGroup},
			pods:             []*v1.Pod{basicPod1},
			wantKey:          fwk.PodGroupKey("default", "basic-pg"),
			wantSuccess:      true,
			wantPodResults:   1,
			expectedNodePods: map[string]sets.Set[string]{"node1": sets.New("p1")},
		},
		{
			name:                    "Single podgroup with basic policy succeeds when only a subset of pods fits",
			nodes:                   []*v1.Node{node4CPU},
			podGroups:               []*schedulingv1beta1.PodGroup{basicPodGroup},
			pods:                    []*v1.Pod{basicPod1, basicPod2},
			wantKey:                 fwk.PodGroupKey("default", "basic-pg"),
			wantSuccess:             true,
			wantPodResults:          2,
			expectedNodePods:        map[string]sets.Set[string]{"node1": sets.New("p1")},
			expectedUnscheduledPods: sets.New("p2"),
		},
		{
			name:               "2-Level composite podgroup with gang policy",
			nodes:              []*v1.Node{node8CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{compositePodGroup1},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroup1leaf1, compositePodGroup1leaf2},
			pods:               []*v1.Pod{compositePodGroup1leaf1Pod2CPU, compositePodGroup1leaf2Pod2CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "gang-cpg1"),
			wantSuccess:        true,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New("p1", "p2")},
		},
		{
			name:               "2-Level composite podgroup with gang policy succeeds when minGroupCount subset fits",
			nodes:              []*v1.Node{node4CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{compositePodGroupMin1},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroupMin1leaf1, compositePodGroupMin1leaf2},
			pods:               []*v1.Pod{compositePodGroupMin1leaf1Pod3CPU, compositePodGroupMin1leaf2Pod3CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "gang-cpg-min1"),
			wantSuccess:        true,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New("p1")},
		},
		{
			name:               "2-Level composite podgroup with gang policy fails early via PlacementFeasible when fewer children than minGroupCount",
			nodes:              []*v1.Node{node8CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{compositePodGroupMin3},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroupMin3leaf1, compositePodGroupMin3leaf2},
			pods:               []*v1.Pod{compositePodGroupMin3leaf1Pod2CPU, compositePodGroupMin3leaf2Pod2CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "gang-cpg-min3"),
			wantSuccess:        false,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New[string]()},
			expectedSkippedKeys: []fwk.EntityKey{
				fwk.PodGroupKey("default", "cpg-min3-leaf1"),
				fwk.PodGroupKey("default", "cpg-min3-leaf2"),
			},
		},
		{
			name:               "2-Level composite podgroup with gang policy terminates loop early when minGroupCount can no longer be met",
			nodes:              []*v1.Node{node4CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{compositePodGroupMin3},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroupMin3leaf1, compositePodGroupMin3leaf2, compositePodGroupMin3leaf3},
			pods:               []*v1.Pod{compositePodGroupMin3leaf1Pod3CPU, compositePodGroupMin3leaf2Pod3CPU, compositePodGroupMin3leaf3Pod3CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "gang-cpg-min3"),
			wantSuccess:        false,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New[string]()},
			expectedSkippedKeys: []fwk.EntityKey{
				fwk.PodGroupKey("default", "cpg-min3-leaf3"),
			},
		},
		{
			name:               "2-Level composite podgroup scheduling failure due to insufficient capacity",
			nodes:              []*v1.Node{node4CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{compositePodGroup1},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroup1leaf1, compositePodGroup1leaf2},
			pods:               []*v1.Pod{compositePodGroup1leaf1Pod3CPU, compositePodGroup1leaf2Pod3CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "gang-cpg1"),
			wantSuccess:        false,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name:               "2-Level composite podgroup with basic policy",
			nodes:              []*v1.Node{node8CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{basicCompositePodGroup},
			podGroups:          []*schedulingv1beta1.PodGroup{basicLeaf1, basicLeaf2},
			pods:               []*v1.Pod{basicLeaf1Pod, basicLeaf2Pod},
			wantKey:            fwk.CompositePodGroupKey("default", "root-basic"),
			wantSuccess:        true,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New("p1", "p2")},
		},
		{
			name:               "Multi-level hierarchy",
			nodes:              []*v1.Node{node8CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCompositePodGroup, compositePodGroup2},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroup2leaf, rootCompositePodGroupleaf},
			pods:               []*v1.Pod{compositePodGroup2leafPod, rootCompositePodGroupleafPod},
			wantKey:            fwk.CompositePodGroupKey("default", "root-cpg"),
			wantSuccess:        true,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New("p1", "p2")},
		},
		{
			name:               "Multi-level hierarchy scheduling failure due to insufficient capacity",
			nodes:              []*v1.Node{node4CPU},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCompositePodGroup, compositePodGroup2},
			podGroups:          []*schedulingv1beta1.PodGroup{compositePodGroup2leaf, rootCompositePodGroupleaf},
			pods:               []*v1.Pod{compositePodGroup2leafPod3CPU, rootCompositePodGroupleafPod3CPU},
			wantKey:            fwk.CompositePodGroupKey("default", "root-cpg"),
			wantSuccess:        false,
			expectedNodePods:   map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				nil,
				tt.nodes,
				tt.podGroups,
				tt.compositePodGroups,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			sched := upstreamsync.NewScheduler(snap, 0, 0, 1, nil)
			fwkInst, err := profileMap.FrameworkForPod(&v1.Pod{})
			if err != nil {
				t.Fatalf("FrameworkForPod failed: %v", err)
			}

			rootInfo, err := snapshot.BuildGroupHierarchyForTest(snap, tt.pods)
			if err != nil {
				t.Fatalf("BuildGroupHierarchyForTest failed: %v", err)
			}

			results, revertFn := sched.RunRootSchedulingAlgorithm(ctx, fwkInst, framework.NewCycleState(), rootInfo)
			result := results[tt.wantKey]

			if result == nil {
				t.Fatalf("expected result for key %v in results map, got nil", tt.wantKey)
			}

			if tt.wantSuccess {
				if revertFn == nil {
					t.Fatalf("expected non-nil revertFn on success")
				}
				if !result.Status.IsSuccess() {
					t.Fatalf("expected pod group scheduling to succeed, got %v", result)
				}
			} else {
				if revertFn != nil {
					t.Fatalf("expected nil revertFn on failure, got non-nil")
				}
				if result.Status.IsSuccess() {
					t.Fatalf("expected scheduling failure, got success: %v", result)
				}
			}

			if len(result.PodResults) != tt.wantPodResults {
				t.Fatalf("expected %d pod results, got %d", tt.wantPodResults, len(result.PodResults))
			}
			for _, pr := range result.PodResults {
				if tt.expectedUnscheduledPods.Has(pr.GetPod().Name) {
					if pr.GetStatus().IsSuccess() {
						t.Errorf("expected pod %s to be unscheduled, got success on node %s", pr.GetPod().Name, pr.GetNodeName())
					}
				} else if !pr.GetStatus().IsSuccess() {
					t.Errorf("expected pod %s success, got %v", pr.GetPod().Name, pr.GetStatus())
				}
			}
			for _, skippedKey := range tt.expectedSkippedKeys {
				if _, exists := results[skippedKey]; exists {
					t.Errorf("expected child group %v to be skipped by early PlacementFeasible termination, but found in results", skippedKey)
				}
			}

			ft.VerifySnapshot(t, snap, tt.expectedNodePods)
		})
	}
}

func TestBuildPodGroupSchedulingFunc(t *testing.T) {
	ctx := context.Background()

	node4CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	compositePodGroup := testutils.MakeGangCompositePodGroup("gang-cpg", "", 2)
	leaf1 := testutils.MakeGangPodGroup("cpg-leaf1", "gang-cpg", 1)
	leaf2 := testutils.MakeGangPodGroup("cpg-leaf2", "gang-cpg", 1)
	pod1 := testutils.MakePod("p1", "cpg-leaf1", "3")
	pod2 := testutils.MakePod("p2", "cpg-leaf2", "3")

	tests := []struct {
		name            string
		nodes           []*v1.Node
		wantSuccess     bool
		wantAssignedSet sets.Set[string]
	}{
		{
			name:            "Composite pod group succeeds and reverts reservations in snapshot",
			nodes:           []*v1.Node{node8CPU},
			wantSuccess:     true,
			wantAssignedSet: sets.New("p1", "p2"),
		},
		{
			name:        "Composite pod group fails when capacity is insufficient",
			nodes:       []*v1.Node{node4CPU},
			wantSuccess: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				nil,
				tt.nodes,
				[]*schedulingv1beta1.PodGroup{leaf1, leaf2},
				[]*schedulingv1alpha3.CompositePodGroup{compositePodGroup},
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			sched := upstreamsync.NewScheduler(snap, 0, 0, 1, nil)
			fwkInst, err := profileMap.FrameworkForPod(&v1.Pod{})
			if err != nil {
				t.Fatalf("FrameworkForPod failed: %v", err)
			}

			rootInfo, err := snapshot.BuildGroupHierarchyForTest(snap, []*v1.Pod{pod1, pod2})
			if err != nil {
				t.Fatalf("BuildGroupHierarchyForTest failed: %v", err)
			}

			schedFn := sched.BuildPodGroupSchedulingFunc(fwkInst, rootInfo)
			assignments, status := schedFn(ctx)

			if tt.wantSuccess {
				if !status.IsSuccess() {
					t.Fatalf("expected scheduling func to succeed, got %v", status)
				}
				if assignments == nil {
					t.Fatalf("expected non-nil PodGroupAssignments")
				}
				gotPods := sets.New[string]()
				for _, pa := range assignments.ProposedAssignments {
					if pa.GetNodeName() != "node1" {
						t.Errorf("expected node1 for pod %s, got %s", pa.GetPod().Name, pa.GetNodeName())
					}
					gotPods.Insert(pa.GetPod().Name)
				}
				if !gotPods.Equal(tt.wantAssignedSet) {
					t.Errorf("got proposed pods %v, want %v", gotPods.UnsortedList(), tt.wantAssignedSet.UnsortedList())
				}
			} else {
				if status.IsSuccess() {
					t.Fatalf("expected scheduling func failure, got success")
				}
				if assignments != nil {
					t.Errorf("expected nil assignments on failure, got %v", assignments)
				}
			}

			// BuildPodGroupSchedulingFunc must always revert reservations in the snapshot.
			ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New[string]()})
		})
	}
}
