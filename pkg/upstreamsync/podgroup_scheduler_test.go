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
	gangPod1 := testutils.MakePod("p1", "gang-pg", "3")
	gangPod2 := testutils.MakePod("p2", "gang-pg", "3")

	basicPodGroup := testutils.MakeBasicPodGroup("basic-pg", "")
	basicPod1 := testutils.MakePod("p1", "basic-pg", "3")

	compositePodGroup1 := testutils.MakeGangCompositePodGroup("gang-cpg1", "", 2)
	compositePodGroup1leaf1 := testutils.MakeGangPodGroup("cpg1-leaf1", "gang-cpg1", 1)
	compositePodGroup1leaf2 := testutils.MakeGangPodGroup("cpg1-leaf2", "gang-cpg1", 1)
	compositePodGroup1leaf1Pod2CPU := testutils.MakePod("p1", "cpg1-leaf1", "2")
	compositePodGroup1leaf2Pod2CPU := testutils.MakePod("p2", "cpg1-leaf2", "2")
	compositePodGroup1leaf1Pod3CPU := testutils.MakePod("p1", "cpg1-leaf1", "3")
	compositePodGroup1leaf2Pod3CPU := testutils.MakePod("p2", "cpg1-leaf2", "3")

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
		name               string
		nodes              []*v1.Node
		podGroups          []*schedulingv1beta1.PodGroup
		compositePodGroups []*schedulingv1alpha3.CompositePodGroup
		pods               []*v1.Pod
		wantKey            fwk.EntityKey
		wantSuccess        bool
		wantPodResults     int
		expectedNodePods   map[string]sets.Set[string]
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
			name:             "Single podgroup scheduling failure due to insufficient capacity",
			nodes:            []*v1.Node{node4CPU},
			podGroups:        []*schedulingv1beta1.PodGroup{gangPodGroup},
			pods:             []*v1.Pod{gangPod1, gangPod2},
			wantKey:          fwk.PodGroupKey("default", "gang-pg"),
			wantSuccess:      false,
			expectedNodePods: map[string]sets.Set[string]{"node1": sets.New[string]()},
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
				t,
				ctx,
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
				if tt.wantPodResults > 0 {
					if len(result.PodResults) != tt.wantPodResults {
						t.Fatalf("expected %d pod results, got %d", tt.wantPodResults, len(result.PodResults))
					}
					for _, pr := range result.PodResults {
						if !pr.GetStatus().IsSuccess() {
							t.Errorf("expected pod %s success, got %v", pr.GetPod().Name, pr.GetStatus())
						}
					}
				}
			} else {
				if revertFn != nil {
					t.Fatalf("expected nil revertFn on failure, got non-nil")
				}
				if result.Status.IsSuccess() {
					t.Fatalf("expected scheduling failure, got success: %v", result)
				}
			}

			ft.VerifySnapshot(t, snap, tt.expectedNodePods)
		})
	}
}
