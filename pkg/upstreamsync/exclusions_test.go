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
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

const fakeNarrowingPluginName = "FakeNarrowing"

type fakeNarrowingPlugin struct {
	nodeNames sets.Set[string]
}

var _ fwk.PreFilterPlugin = &fakeNarrowingPlugin{}

func (pl *fakeNarrowingPlugin) Name() string {
	return fakeNarrowingPluginName
}

func (pl *fakeNarrowingPlugin) PreFilter(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ []fwk.NodeInfo) (*fwk.PreFilterResult, *fwk.Status) {
	if pl.nodeNames == nil {
		return nil, fwk.NewStatus(fwk.Skip)
	}
	return &fwk.PreFilterResult{NodeNames: pl.nodeNames.Clone()}, nil
}

func (pl *fakeNarrowingPlugin) PreFilterExtensions() fwk.PreFilterExtensions {
	return nil
}

func exclusionsTestProfile() schedulerapi.KubeSchedulerProfile {
	return schedulerapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedulerapi.Plugins{
			QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
			PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}, {Name: "NodeAffinity"}, {Name: fakeNarrowingPluginName}}},
			Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}, {Name: "NodeAffinity"}}},
			Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
		},
		PluginConfig: []schedulerapi.PluginConfig{
			{
				Name: "NodeResourcesFit",
				Args: &schedulerapi.NodeResourcesFitArgs{
					ScoringStrategy: &schedulerapi.ScoringStrategy{Type: schedulerapi.LeastAllocated},
				},
			},
			{
				Name: "NodeAffinity",
				Args: &schedulerapi.NodeAffinityArgs{},
			},
		},
	}
}

func newFilterExtender(t *testing.T, rejected sets.Set[string]) schedulerapi.Extender {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args extenderv1.ExtenderArgs
		if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := extenderv1.ExtenderFilterResult{Nodes: &v1.NodeList{}, FailedNodes: extenderv1.FailedNodesMap{}}
		for _, node := range args.Nodes.Items {
			if rejected.Has(node.Name) {
				result.FailedNodes[node.Name] = "rejected by the test extender"
			} else {
				result.Nodes.Items = append(result.Nodes.Items, node)
			}
		}
		if err := json.NewEncoder(w).Encode(&result); err != nil {
			t.Errorf("Failed to encode the extender filter result: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return schedulerapi.Extender{URLPrefix: server.URL, FilterVerb: "filter"}
}

// nodeNameAffinity ORs the terms and ANDs the node names within a term. Each requirement carries a
// single node name, as the API only accepts one value for metadata.name selectors.
func nodeNameAffinity(nodeNamesPerTerm ...[]string) *v1.NodeAffinity {
	var terms []v1.NodeSelectorTerm
	for _, nodeNames := range nodeNamesPerTerm {
		var term v1.NodeSelectorTerm
		for _, nodeName := range nodeNames {
			term.MatchFields = append(term.MatchFields, v1.NodeSelectorRequirement{
				Key:      metav1.ObjectNameField,
				Operator: v1.NodeSelectorOpIn,
				Values:   []string{nodeName},
			})
		}
		terms = append(terms, term)
	}
	return &v1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{NodeSelectorTerms: terms},
	}
}

func newPendingPod(t *testing.T, pod *v1.Pod) *upstreamsync.PendingPod {
	t.Helper()
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatalf("Failed to create the pod info: %v", err)
	}
	return &upstreamsync.PendingPod{PodInfo: podInfo, CycleState: framework.NewCycleState()}
}

func nodeNames(nodeInfos []fwk.NodeInfo) []string {
	names := make([]string, 0, len(nodeInfos))
	for _, nodeInfo := range nodeInfos {
		names = append(names, nodeInfo.Node().Name)
	}
	slices.Sort(names)
	return names
}

// ClusterSnapshot.CanSchedulePodWithExclusions relies on the records checked here.
func TestFindAllNodesThatFitPodWithExclusions(t *testing.T) {
	placementNodes := []string{"node1", "node2", "node3"}
	tests := []struct {
		name                     string
		nodeCPU                  map[string]string
		podCPU                   string
		nodeAffinity             *v1.NodeAffinity
		narrowTo                 sets.Set[string]
		extenderRejects          sets.Set[string]
		wantFeasible             []string
		wantExclusions           []upstreamsync.NodeExclusion
		wantUnschedulablePlugins sets.Set[string]
	}{
		{
			name:         "every node fits",
			wantFeasible: placementNodes,
		},
		{
			name:         "Filter rejection names the plugin that rejected the node",
			nodeCPU:      map[string]string{"node1": "0"},
			podCPU:       "1",
			wantFeasible: []string{"node2", "node3"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStageFilter, Plugins: []string{"NodeResourcesFit"}},
			},
		},
		{
			name:         "PreFilter rejection excludes every node",
			nodeAffinity: nodeNameAffinity([]string{"node1", "node2"}),
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStagePreFilter, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilter, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilter, Plugins: []string{"NodeAffinity"}},
			},
		},
		{
			name:         "narrowing names the plugin that narrowed",
			nodeAffinity: nodeNameAffinity([]string{"node1"}),
			wantFeasible: []string{"node1"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
			},
		},
		{
			name:                     "narrowing stays attributed when Filter rejects the remaining node",
			nodeCPU:                  map[string]string{"node1": "0"},
			podCPU:                   "1",
			nodeAffinity:             nodeNameAffinity([]string{"node1"}),
			wantUnschedulablePlugins: sets.New("NodeAffinity", "NodeResourcesFit"),
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStageFilter, Plugins: []string{"NodeResourcesFit"}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
			},
		},
		{
			name:         "narrowing by several plugins names all of them",
			nodeAffinity: nodeNameAffinity([]string{"node1"}),
			narrowTo:     sets.New("node1", "node2"),
			wantFeasible: []string{"node1"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName, "NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName, "NodeAffinity"}},
			},
		},
		{
			name:     "narrowing down to no node excludes every node",
			narrowTo: sets.New[string](),
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName}},
			},
		},
		{
			name:         "narrowings by several plugins that do not intersect exclude every node",
			nodeAffinity: nodeNameAffinity([]string{"node1"}),
			narrowTo:     sets.New("node2"),
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName, "NodeAffinity"}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName, "NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{fakeNarrowingPluginName, "NodeAffinity"}},
			},
		},
		{
			name:         "narrowing down to nodes outside of the placement excludes every node",
			nodeAffinity: nodeNameAffinity([]string{"node4"}),
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
				{NodeName: "node3", Stage: upstreamsync.RejectionStagePreFilterNarrowing, Plugins: []string{"NodeAffinity"}},
			},
		},
		{
			name:            "extender rejection names no plugin",
			extenderRejects: sets.New("node2"),
			wantFeasible:    []string{"node1", "node3"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node2", Stage: upstreamsync.RejectionStageExtender},
			},
		},
		{
			name:            "Filter and extender rejections are told apart",
			nodeCPU:         map[string]string{"node1": "0"},
			podCPU:          "1",
			extenderRejects: sets.New("node2"),
			wantFeasible:    []string{"node3"},
			wantExclusions: []upstreamsync.NodeExclusion{
				{NodeName: "node1", Stage: upstreamsync.RejectionStageFilter, Plugins: []string{"NodeResourcesFit"}},
				{NodeName: "node2", Stage: upstreamsync.RejectionStageExtender},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			nodes := make([]*v1.Node, 0, len(placementNodes))
			for _, name := range placementNodes {
				cpu := "2"
				if c, ok := tc.nodeCPU[name]; ok {
					cpu = c
				}
				nodes = append(nodes, st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    cpu,
					v1.ResourceMemory: "1Gi",
					v1.ResourcePods:   "110",
				}).Obj())
			}
			opts := []upstreamsync.Option{
				upstreamsync.WithProfiles(exclusionsTestProfile()),
				upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					fakeNarrowingPluginName: func(context.Context, runtime.Object, fwk.Handle) (fwk.Plugin, error) {
						return &fakeNarrowingPlugin{nodeNames: tc.narrowTo}, nil
					},
				}),
			}
			if tc.extenderRejects != nil {
				opts = append(opts, upstreamsync.WithExtenders(newFilterExtender(t, tc.extenderRejects)))
			}
			profiles, snap, err := ft.SetupSnapshotTest(ctx, nil, nodes, opts...)
			if err != nil {
				t.Fatalf("Failed to set up the snapshot: %v", err)
			}

			podBuilder := st.MakePod().Name("pod").Namespace("default").UID("pod")
			if tc.podCPU != "" {
				podBuilder = podBuilder.Req(map[v1.ResourceName]string{v1.ResourceCPU: tc.podCPU})
			}
			if tc.nodeAffinity != nil {
				podBuilder = podBuilder.NodeAffinity(tc.nodeAffinity)
			}
			pod := podBuilder.Obj()
			schedFramework, err := profiles.FrameworkForPod(pod)
			if err != nil {
				t.Fatalf("Failed to get the framework: %v", err)
			}

			placement := &fwk.Placement{}
			for _, name := range placementNodes {
				nodeInfo, err := snap.NodeInfos().Get(name)
				if err != nil {
					t.Fatalf("Failed to get node %s from the snapshot: %v", name, err)
				}
				placement.Nodes = append(placement.Nodes, nodeInfo)
			}
			if err := snap.AssumePlacement(placement); err != nil {
				t.Fatalf("Failed to assume the placement: %v", err)
			}
			t.Cleanup(snap.ForgetPlacement)

			sched := upstreamsync.NewScheduler(snap, 0, 0, math.MaxInt32, nil)
			feasible, diagnosis, exclusions, _, err := sched.FindAllNodesThatFitPodWithExclusions(ctx, schedFramework, newPendingPod(t, pod))
			if err != nil {
				t.Fatalf("FindAllNodesThatFitPodWithExclusions() error = %v", err)
			}
			if diff := cmp.Diff(tc.wantFeasible, nodeNames(feasible), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected feasible nodes (-want +got):\n%s", diff)
			}
			slices.SortFunc(exclusions, func(a, b upstreamsync.NodeExclusion) int {
				return strings.Compare(a.NodeName, b.NodeName)
			})
			if diff := cmp.Diff(tc.wantExclusions, exclusions, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Unexpected exclusions (-want +got):\n%s", diff)
			}
			if tc.wantUnschedulablePlugins != nil {
				if diff := cmp.Diff(tc.wantUnschedulablePlugins, diagnosis.UnschedulablePlugins); diff != "" {
					t.Errorf("Unexpected Diagnosis.UnschedulablePlugins (-want +got):\n%s", diff)
				}
			}

			plainSched := upstreamsync.NewScheduler(snap, 0, 0, math.MaxInt32, nil)
			plainFeasible, plainDiagnosis, _, err := plainSched.FindAllNodesThatFitPod(ctx, schedFramework, newPendingPod(t, pod))
			if err != nil {
				t.Fatalf("FindAllNodesThatFitPod() error = %v", err)
			}
			if diff := cmp.Diff(nodeNames(plainFeasible), nodeNames(feasible)); diff != "" {
				t.Errorf("Feasible nodes differ from FindAllNodesThatFitPod (-plain +withExclusions):\n%s", diff)
			}
			if diff := cmp.Diff(plainDiagnosis.UnschedulablePlugins, diagnosis.UnschedulablePlugins); diff != "" {
				t.Errorf("Diagnosis.UnschedulablePlugins differs from FindAllNodesThatFitPod (-plain +withExclusions):\n%s", diff)
			}
		})
	}
}
