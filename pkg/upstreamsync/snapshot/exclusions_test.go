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

package snapshot

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	st "k8s.io/kubernetes/pkg/scheduler/testing"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

func testPlacement(nodeNames ...string) *fwk.Placement {
	nodes := make([]fwk.NodeInfo, 0, len(nodeNames))
	for _, name := range nodeNames {
		ni := framework.NewNodeInfo()
		ni.SetNode(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		nodes = append(nodes, ni)
	}
	return &fwk.Placement{Nodes: nodes}
}

func statusRejectedBy(plugin string) *fwk.Status {
	return fwk.NewStatus(fwk.UnschedulableAndUnresolvable).WithPlugin(plugin)
}

func TestNodeExclusionsFromDiagnosis(t *testing.T) {
	tests := []struct {
		name      string
		placement *fwk.Placement
		feasible  []string
		diag      *framework.Diagnosis
		want      []NodeExclusion
	}{
		{
			name:      "filter rejections attributed per plugin in placement order",
			placement: testPlacement("n1", "n2", "n3", "n4"),
			feasible:  []string{"n4"},
			diag: func() *framework.Diagnosis {
				nts := framework.NewDefaultNodeToStatus()
				nts.Set("n1", statusRejectedBy("TaintToleration"))
				nts.Set("n2", statusRejectedBy("TaintToleration"))
				nts.Set("n3", statusRejectedBy("NodeAffinity"))
				return &framework.Diagnosis{NodeToStatus: nts}
			}(),
			want: []NodeExclusion{
				{NodeName: "n1", Plugin: "TaintToleration", Stage: RejectionStageFilter},
				{NodeName: "n2", Plugin: "TaintToleration", Stage: RejectionStageFilter},
				{NodeName: "n3", Plugin: "NodeAffinity", Stage: RejectionStageFilter},
			},
		},
		{
			name:      "prefilter plugin rejection covers all nodes via the absent status",
			placement: testPlacement("n1", "n2"),
			feasible:  nil,
			diag: func() *framework.Diagnosis {
				nts := framework.NewDefaultNodeToStatus()
				nts.SetAbsentNodesStatus(statusRejectedBy("NodeAffinity"))
				return &framework.Diagnosis{NodeToStatus: nts}
			}(),
			want: []NodeExclusion{
				{NodeName: "n1", Plugin: "NodeAffinity", Stage: RejectionStagePreFilter},
				{NodeName: "n2", Plugin: "NodeAffinity", Stage: RejectionStagePreFilter},
			},
		},
		{
			name:      "narrowing by exactly one plugin is attributed to it",
			placement: testPlacement("n1", "n2"),
			feasible:  nil,
			diag: &framework.Diagnosis{
				NodeToStatus:         framework.NewDefaultNodeToStatus(),
				UnschedulablePlugins: sets.New("NodeAffinity"),
			},
			want: []NodeExclusion{
				{NodeName: "n1", Plugin: "NodeAffinity", Stage: RejectionStagePreFilterNarrowing},
				{NodeName: "n2", Plugin: "NodeAffinity", Stage: RejectionStagePreFilterNarrowing},
			},
		},
		{
			name:      "narrowing by several plugins is never guessed",
			placement: testPlacement("n1"),
			feasible:  nil,
			diag: &framework.Diagnosis{
				NodeToStatus:         framework.NewDefaultNodeToStatus(),
				UnschedulablePlugins: sets.New("NodeAffinity", "OtherPlugin"),
			},
			want: []NodeExclusion{
				{NodeName: "n1", Stage: RejectionStagePreFilterNarrowing},
			},
		},
		{
			name:      "extender rejections carry the extender stage without a plugin",
			placement: testPlacement("n1", "n2"),
			feasible:  []string{"n2"},
			diag: func() *framework.Diagnosis {
				nts := framework.NewDefaultNodeToStatus()
				nts.Set("n1", fwk.NewStatus(fwk.UnschedulableAndUnresolvable))
				return &framework.Diagnosis{
					NodeToStatus:         nts,
					UnschedulablePlugins: sets.New(framework.ExtenderName),
				}
			}(),
			want: []NodeExclusion{
				{NodeName: "n1", Stage: RejectionStageExtender},
			},
		},
		{
			name:      "extender marker does not masquerade as a narrowing plugin",
			placement: testPlacement("n1", "n2"),
			feasible:  nil,
			diag: func() *framework.Diagnosis {
				nts := framework.NewDefaultNodeToStatus()
				nts.Set("n1", fwk.NewStatus(fwk.UnschedulableAndUnresolvable))
				return &framework.Diagnosis{
					NodeToStatus:         nts,
					UnschedulablePlugins: sets.New(framework.ExtenderName, "NodeAffinity"),
				}
			}(),
			want: []NodeExclusion{
				{NodeName: "n1", Stage: RejectionStageExtender},
				{NodeName: "n2", Plugin: "NodeAffinity", Stage: RejectionStagePreFilterNarrowing},
			},
		},
		{
			name:      "unknown plugin names pass through verbatim",
			placement: testPlacement("n1"),
			feasible:  nil,
			diag: func() *framework.Diagnosis {
				nts := framework.NewDefaultNodeToStatus()
				nts.Set("n1", statusRejectedBy("SomeFuturePlugin"))
				return &framework.Diagnosis{NodeToStatus: nts}
			}(),
			want: []NodeExclusion{
				{NodeName: "n1", Plugin: "SomeFuturePlugin", Stage: RejectionStageFilter},
			},
		},
		{
			name:      "no diagnosis data yields unknown-stage records",
			placement: testPlacement("n1", "n2"),
			feasible:  nil,
			diag:      nil,
			want: []NodeExclusion{
				{NodeName: "n1", Stage: RejectionStageUnknown},
				{NodeName: "n2", Stage: RejectionStageUnknown},
			},
		},
		{
			name:      "all nodes feasible yields no records",
			placement: testPlacement("n1", "n2"),
			feasible:  []string{"n1", "n2"},
			diag:      &framework.Diagnosis{NodeToStatus: framework.NewDefaultNodeToStatus()},
			want:      nil,
		},
		{
			name:      "nil placement yields no records",
			placement: nil,
			feasible:  nil,
			diag:      &framework.Diagnosis{NodeToStatus: framework.NewDefaultNodeToStatus()},
			want:      nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NodeExclusionsFromDiagnosis(tc.placement, tc.feasible, tc.diag)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("unexpected exclusions (-want +got):\n%s", diff)
			}

			rejected := 0
			if tc.placement != nil {
				feasibleSet := sets.New(tc.feasible...)
				for _, ni := range tc.placement.Nodes {
					if !feasibleSet.Has(ni.Node().Name) {
						rejected++
					}
				}
			}
			if len(got) != rejected {
				t.Errorf("got %d records for %d rejected nodes (every rejected node must have exactly one record)", len(got), rejected)
			}

			if again := NodeExclusionsFromDiagnosis(tc.placement, tc.feasible, tc.diag); !cmp.Equal(got, again) {
				t.Errorf("derivation is not deterministic:\nfirst: %v\nsecond: %v", got, again)
			}

			if tc.diag != nil && tc.diag.NodeToStatus != nil {
				lenBefore := tc.diag.NodeToStatus.Len()
				_ = NodeExclusionsFromDiagnosis(tc.placement, tc.feasible, tc.diag)
				if tc.diag.NodeToStatus.Len() != lenBefore {
					t.Errorf("derivation mutated the diagnosis NodeToStatus (len %d -> %d)", lenBefore, tc.diag.NodeToStatus.Len())
				}
			}
		})
	}
}

func TestCanSchedulePodWithExclusions(t *testing.T) {
	tests := []struct {
		name           string
		candidateNodes []string
		podRequestCPU  string
		expectNodes    []string
		expectExcluded []NodeExclusion
	}{
		{
			name:           "all nodes feasible yields no exclusions",
			candidateNodes: []string{"node1", "node2"},
			expectNodes:    []string{"node1", "node2"},
			expectExcluded: nil,
		},
		{
			name:           "filter rejection is attributed to the rejecting plugin",
			candidateNodes: []string{"node1"},
			podRequestCPU:  "1",
			expectNodes:    []string{},
			expectExcluded: []NodeExclusion{
				{NodeName: "node1", Plugin: "NodeResourcesFit", Stage: RejectionStageFilter},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			snapshotNodes := make([]*v1.Node, len(tc.candidateNodes))
			for i, name := range tc.candidateNodes {
				snapshotNodes[i] = st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    "0",
					v1.ResourceMemory: "0",
					v1.ResourcePods:   "110",
				}).Obj()
			}
			cs, _, _ := setupSnapshotTest(t, ctx, snapshotNodes, nil)

			podBuilder := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1")
			if tc.podRequestCPU != "" {
				podBuilder = podBuilder.Req(map[v1.ResourceName]string{v1.ResourceCPU: tc.podRequestCPU})
			}
			pod := podBuilder.Obj()

			placement, err := cs.MakePlacement(tc.candidateNodes)
			if err != nil {
				t.Fatalf("MakePlacement() error = %v", err)
			}

			feasible, exclusions, _, err := cs.CanSchedulePodWithExclusions(ctx, pod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePodWithExclusions() error = %v", err)
			}

			plainFeasible, _, err := cs.CanSchedulePod(ctx, pod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePod() error = %v", err)
			}
			if diff := cmp.Diff(plainFeasible, feasible); diff != "" {
				t.Errorf("feasible nodes differ from CanSchedulePod (-plain +withExclusions):\n%s", diff)
			}
			if diff := cmp.Diff(tc.expectNodes, feasible); diff != "" {
				t.Errorf("unexpected feasible nodes (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tc.expectExcluded, exclusions); diff != "" {
				t.Errorf("unexpected exclusions (-want +got):\n%s", diff)
			}
		})
	}
}

func BenchmarkNodeExclusionsFromDiagnosis(b *testing.B) {
	const numNodes = 1000
	nodeNames := make([]string, numNodes)
	for i := range numNodes {
		nodeNames[i] = fmt.Sprintf("node-%d", i)
	}
	placement := testPlacement(nodeNames...)
	nts := framework.NewDefaultNodeToStatus()
	for i, name := range nodeNames {
		switch i % 3 {
		case 0:
			nts.Set(name, statusRejectedBy("TaintToleration"))
		case 1:
			nts.Set(name, statusRejectedBy("NodeAffinity"))
			// case 2: resolved through the absent status.
		}
	}
	nts.SetAbsentNodesStatus(statusRejectedBy("NodeAffinity"))
	diag := &framework.Diagnosis{NodeToStatus: nts}

	for b.Loop() {
		if got := NodeExclusionsFromDiagnosis(placement, nil, diag); len(got) != numNodes {
			b.Fatalf("expected %d records, got %d", numNodes, len(got))
		}
	}
}
