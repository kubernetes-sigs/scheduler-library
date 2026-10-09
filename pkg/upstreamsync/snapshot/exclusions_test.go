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
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

func TestCanSchedulePodWithExclusions(t *testing.T) {
	ctx := t.Context()
	nodeWithCPU := func(name, cpu string) *v1.Node {
		return st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
			v1.ResourceCPU:    cpu,
			v1.ResourceMemory: "1Gi",
			v1.ResourcePods:   "110",
		}).Obj()
	}
	cs, _, _ := setupSnapshotTest(ctx, t, []*v1.Node{nodeWithCPU("node1", "0"), nodeWithCPU("node2", "2")}, nil)

	tests := []struct {
		name           string
		candidateNodes []string
		podCPU         string
		wantFeasible   []string
		wantExclusions []upstreamsync.NodeExclusion
	}{
		{
			name: "nil placement",
		},
		{
			name:           "empty placement",
			candidateNodes: []string{},
		},
		{
			name:           "every node fits",
			candidateNodes: []string{"node1", "node2"},
			wantFeasible:   []string{"node1", "node2"},
		},
		{
			name:           "the nodes that do not fit come with the records of the scheduling flow",
			candidateNodes: []string{"node1", "node2"},
			podCPU:         "1",
			wantFeasible:   []string{"node2"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStageFilter, Plugins: []string{"NodeResourcesFit"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			podBuilder := st.MakePod().Name("pod").Namespace("default").UID("pod")
			if tc.podCPU != "" {
				podBuilder = podBuilder.Req(map[v1.ResourceName]string{v1.ResourceCPU: tc.podCPU})
			}
			pod := podBuilder.Obj()
			var placement *fwk.Placement
			if tc.candidateNodes != nil {
				var err error
				placement, err = cs.MakePlacement(tc.candidateNodes)
				if err != nil {
					t.Fatalf("MakePlacement() error = %v", err)
				}
			}

			feasible, diagnosis, exclusions, err := cs.CanSchedulePodWithExclusions(ctx, pod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePodWithExclusions() error = %v", err)
			}
			slices.Sort(feasible)
			if diff := cmp.Diff(tc.wantFeasible, feasible, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected feasible nodes (-want +got):\n%s", diff)
			}
			slices.SortFunc(exclusions, func(a, b upstreamsync.NodeExclusion) int {
				return strings.Compare(a.NodeName, b.NodeName)
			})
			if diff := cmp.Diff(tc.wantExclusions, exclusions, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected exclusions (-want +got):\n%s", diff)
			}

			plainFeasible, plainDiagnosis, err := cs.CanSchedulePod(ctx, pod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePod() error = %v", err)
			}
			slices.Sort(plainFeasible)
			if diff := cmp.Diff(plainFeasible, feasible); diff != "" {
				t.Errorf("Feasible nodes differ from CanSchedulePod (-plain +withExclusions):\n%s", diff)
			}
			if (plainDiagnosis == nil) != (diagnosis == nil) {
				t.Fatalf("Diagnosis presence differs from CanSchedulePod: plain %v, withExclusions %v", plainDiagnosis, diagnosis)
			}
			if diagnosis != nil {
				if diff := cmp.Diff(plainDiagnosis.UnschedulablePlugins, diagnosis.UnschedulablePlugins); diff != "" {
					t.Errorf("Diagnosis.UnschedulablePlugins differs from CanSchedulePod (-plain +withExclusions):\n%s", diff)
				}
			}
		})
	}
}
