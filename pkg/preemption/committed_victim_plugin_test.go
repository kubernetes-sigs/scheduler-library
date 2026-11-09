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
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/preemption"
)

func TestCommittedVictimPlugin_Filter(t *testing.T) {
	tests := []struct {
		name        string
		cycleState  fwk.CycleState
		nodes       []*v1.Node
		wantResults map[string]fwk.Code
	}{
		{
			name: "unset data in cycle state allows all nodes to pass",
			cycleState: func() fwk.CycleState {
				c := framework.NewCycleState()
				c.SetPodGroupSchedulingCycle(framework.NewCycleState())
				return c
			}(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Obj(),
				st.MakeNode().Name("node3").Obj(),
			},
			wantResults: map[string]fwk.Code{
				"node1": fwk.Success,
				"node2": fwk.Success,
				"node3": fwk.Success,
			},
		},
		{
			name:       "cycle state without pod group allows all nodes to pass",
			cycleState: framework.NewCycleState(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Obj(),
				st.MakeNode().Name("node3").Obj(),
			},
			wantResults: map[string]fwk.Code{
				"node1": fwk.Success,
				"node2": fwk.Success,
				"node3": fwk.Success,
			},
		},
		{
			name: "empty nodes with victims allows all nodes to pass",
			cycleState: func() fwk.CycleState {
				c := framework.NewCycleState()
				pgc := framework.NewCycleState()
				if err := preemption.SetCommittedVictimsInCycleState(pgc, []preemption.PreemptionVictim{}); err != nil {
					t.Fatalf("SetCommittedVictimsInCycleState failed: %v", err)
				}
				c.SetPodGroupSchedulingCycle(pgc)
				return c
			}(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Obj(),
				st.MakeNode().Name("node3").Obj(),
			},
			wantResults: map[string]fwk.Code{
				"node1": fwk.Success,
				"node2": fwk.Success,
				"node3": fwk.Success,
			},
		},
		{
			name: "returns unschedulable for nodes with victims",
			cycleState: func() fwk.CycleState {
				c := framework.NewCycleState()
				pgc := framework.NewCycleState()
				if err := preemption.SetCommittedVictimsInCycleState(pgc, []preemption.PreemptionVictim{
					&testVictim{
						pods: []*v1.Pod{
							st.MakePod().Name("v1").Node("node1").Obj(),
							st.MakePod().Name("v2").Node("node3").Obj(),
						},
					},
					&testVictim{
						pods: []*v1.Pod{
							st.MakePod().Name("v3").Node("node3").Obj(),
							st.MakePod().Name("v4").Node("node4").Obj(),
						},
					},
				}); err != nil {
					t.Fatalf("SetCommittedVictimsInCycleState failed: %v", err)
				}
				c.SetPodGroupSchedulingCycle(pgc)
				return c
			}(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Obj(),
				st.MakeNode().Name("node3").Obj(),
				st.MakeNode().Name("node4").Obj(),
			},
			wantResults: map[string]fwk.Code{
				"node1": fwk.Unschedulable,
				"node2": fwk.Success,
				"node3": fwk.Unschedulable,
				"node4": fwk.Unschedulable,
			},
		},
		{
			name: "walks nested parent placement cycle states to root pod group cycle state",
			cycleState: func() fwk.CycleState {
				rootPGC := framework.NewCycleState()
				if err := preemption.SetCommittedVictimsInCycleState(rootPGC, []preemption.PreemptionVictim{
					&testVictim{
						pods: []*v1.Pod{
							st.MakePod().Name("v1").Node("node2").Obj(),
						},
					},
				}); err != nil {
					t.Fatalf("SetCommittedVictimsInCycleState failed: %v", err)
				}

				rootPlacement := framework.NewCycleState()
				rootPlacement.SetPodGroupSchedulingCycle(rootPGC)

				midPGC := framework.NewCycleState()
				midPGC.SetPlacementCycleState(rootPlacement)

				midPlacement := framework.NewCycleState()
				midPlacement.SetPodGroupSchedulingCycle(midPGC)

				leafPGC := framework.NewCycleState()
				leafPGC.SetPlacementCycleState(midPlacement)

				podState := framework.NewCycleState()
				podState.SetPodGroupSchedulingCycle(leafPGC)
				return podState
			}(),
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
				st.MakeNode().Name("node2").Obj(),
				st.MakeNode().Name("node3").Obj(),
			},
			wantResults: map[string]fwk.Code{
				"node1": fwk.Success,
				"node2": fwk.Unschedulable,
				"node3": fwk.Success,
			},
		},
	}

	plugin := &preemption.CommittedVictimPlugin{}
	testPod := st.MakePod().Name("test-pod").Obj()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotResults := map[string]fwk.Code{}

			for _, node := range tt.nodes {
				ni := framework.NewNodeInfo()
				ni.SetNode(node)
				gotResults[node.Name] = plugin.Filter(t.Context(), tt.cycleState, testPod, ni).Code()
			}

			if diff := cmp.Diff(tt.wantResults, gotResults); diff != "" {
				t.Errorf("unexpected status (-want, +got): %s", diff)
			}
		})
	}
}
