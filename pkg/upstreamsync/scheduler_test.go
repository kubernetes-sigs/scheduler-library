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

package upstreamsync

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	tf "k8s.io/kubernetes/pkg/scheduler/testing/framework"
	"k8s.io/utils/ptr"
)

type testNoopPodNominator struct{}

func (testNoopPodNominator) AddNominatedPod(klog.Logger, fwk.PodInfo, *fwk.NominatingInfo) {}
func (testNoopPodNominator) DeleteNominatedPodIfExists(*v1.Pod)                            {}
func (testNoopPodNominator) UpdateNominatedPod(klog.Logger, *v1.Pod, fwk.PodInfo)          {}
func (testNoopPodNominator) NominatedPodsForNode(string) []fwk.PodInfo                     { return nil }

func TestNumFeasibleNodesToFind(t *testing.T) {
	tests := []struct {
		name              string
		globalPercentage  int32
		profilePercentage *int32
		numAllNodes       int32
		wantNumNodes      int32
	}{
		{
			name:         "not set percentageOfNodesToScore and nodes number not more than 100",
			numAllNodes:  10,
			wantNumNodes: 10,
		},
		{
			name:              "set profile percentageOfNodesToScore and nodes number not more than 100",
			profilePercentage: ptr.To[int32](40),
			numAllNodes:       10,
			wantNumNodes:      10,
		},
		{
			name:         "not set percentageOfNodesToScore and nodes number more than 100",
			numAllNodes:  1000,
			wantNumNodes: 420,
		},
		{
			name:              "set profile percentageOfNodesToScore and nodes number more than 100",
			profilePercentage: ptr.To[int32](40),
			numAllNodes:       1000,
			wantNumNodes:      400,
		},
		{
			name:              "set global and profile percentageOfNodesToScore and nodes number more than 100",
			globalPercentage:  100,
			profilePercentage: ptr.To[int32](40),
			numAllNodes:       1000,
			wantNumNodes:      400,
		},
		{
			name:             "set global percentageOfNodesToScore and nodes number more than 100",
			globalPercentage: 40,
			numAllNodes:      1000,
			wantNumNodes:     400,
		},
		{
			name:         "not set profile percentageOfNodesToScore and nodes number more than 50*125",
			numAllNodes:  6000,
			wantNumNodes: 300,
		},
		{
			name:              "set profile percentageOfNodesToScore and nodes number more than 50*125",
			profilePercentage: ptr.To[int32](40),
			numAllNodes:       6000,
			wantNumNodes:      2400,
		},
		{
			name:              "result below minFeasibleNodesToFind is clamped to 100",
			profilePercentage: ptr.To[int32](5),
			numAllNodes:       500,
			wantNumNodes:      100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched := &Scheduler{
				percentageOfNodesToScore: tt.globalPercentage,
			}
			if gotNumNodes := sched.numFeasibleNodesToFind(tt.profilePercentage, tt.numAllNodes); gotNumNodes != tt.wantNumNodes {
				t.Errorf("Scheduler.numFeasibleNodesToFind() = %v, want %v", gotNumNodes, tt.wantNumNodes)
			}
		})
	}
}

func nodeResourcesFitPluginConfig() []schedulerapi.PluginConfig {
	return []schedulerapi.PluginConfig{
		{
			Name: "NodeResourcesFit",
			Args: &schedulerapi.NodeResourcesFitArgs{
				ScoringStrategy: &schedulerapi.ScoringStrategy{
					Type: schedulerapi.LeastAllocated,
				},
			},
		},
	}
}

func TestNumNodesToFind(t *testing.T) {
	basePlugins := func(withScore bool) *schedulerapi.Plugins {
		plugins := &schedulerapi.Plugins{
			QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
			Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
			Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
		}
		if withScore {
			plugins.PreScore = schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}}
			plugins.Score = schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit", Weight: 1}}}
		}
		return plugins
	}

	tests := []struct {
		name              string
		globalPercentage  int32
		profilePercentage *int32
		withScore         bool
		firstFit          bool
		extenders         []schedulerapi.Extender
		numAllNodes       int32
		wantNumNodes      int32
	}{
		{
			name:         "zero nodes returns zero",
			withScore:    true,
			numAllNodes:  0,
			wantNumNodes: 0,
		},
		{
			name:             "no scoring and no extender filters returns 1",
			globalPercentage: 50,
			withScore:        false,
			numAllNodes:      500,
			wantNumNodes:     1,
		},
		{
			name:         "with score plugin and dynamic percentage",
			withScore:    true,
			numAllNodes:  500,
			wantNumNodes: 230,
		},
		{
			name:              "with score plugin and profile percentage",
			globalPercentage:  100,
			profilePercentage: ptr.To[int32](30),
			withScore:         true,
			numAllNodes:       500,
			wantNumNodes:      150,
		},
		{
			name:             "no score plugin but with extender filter uses calculated percentage",
			globalPercentage: 30,
			withScore:        false,
			extenders: []schedulerapi.Extender{
				{
					URLPrefix:  "http://127.0.0.1:12345",
					FilterVerb: "filter",
				},
			},
			numAllNodes:  500,
			wantNumNodes: 150,
		},
		{
			name:             "first fit with score plugin returns 1",
			globalPercentage: 50,
			withScore:        true,
			firstFit:         true,
			numAllNodes:      500,
			wantNumNodes:     1,
		},
		{
			name:             "first fit with extender filter uses calculated percentage",
			globalPercentage: 30,
			withScore:        true,
			firstFit:         true,
			extenders: []schedulerapi.Extender{
				{
					URLPrefix:  "http://127.0.0.1:12345",
					FilterVerb: "filter",
				},
			},
			numAllNodes:  500,
			wantNumNodes: 150,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)

			prof := schedulerapi.KubeSchedulerProfile{
				SchedulerName:            v1.DefaultSchedulerName,
				PercentageOfNodesToScore: tt.profilePercentage,
				Plugins:                  basePlugins(tt.withScore),
				PluginConfig:             nodeResourcesFitPluginConfig(),
			}
			opts := []Option{WithProfiles(prof)}
			if len(tt.extenders) > 0 {
				opts = append(opts, WithExtenders(tt.extenders...))
			}

			comps, err := NewFrameworkComponents(ctx, client, informerFactory, opts...)
			if err != nil {
				t.Fatalf("NewFrameworkComponents failed: %v", err)
			}
			snap := internalcache.NewEmptySnapshot()
			profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, snap)
			if err != nil {
				t.Fatalf("NewFrameworkMap failed: %v", err)
			}
			fwkInst, err := profileMap.FrameworkForPod(&v1.Pod{})
			if err != nil {
				t.Fatalf("FrameworkForPod failed: %v", err)
			}

			algorithmOpts := []AlgorithmOption{WithAlgorithmPercentageOfNodesToScore(tt.globalPercentage)}
			if tt.firstFit {
				algorithmOpts = append(algorithmOpts, WithFirstFit())
			}
			sched := NewScheduler(snap, 0, 0, nil, algorithmOpts...)
			if got := sched.numNodesToFind(fwkInst, tt.numAllNodes); got != tt.wantNumNodes {
				t.Errorf("numNodesToFind() = %d, want %d", got, tt.wantNumNodes)
			}
		})
	}
}

func TestFindNodesThatPassFilters(t *testing.T) {
	ctx := t.Context()
	const totalNodes = 10
	nodes := make([]*v1.Node, totalNodes)
	for i := range totalNodes {
		nodes[i] = st.MakeNode().Name(fmt.Sprintf("node-%d", i)).Capacity(map[v1.ResourceName]string{
			v1.ResourceCPU:    "4",
			v1.ResourceMemory: "4Gi",
			v1.ResourcePods:   "10",
		}).Obj()
	}

	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	prof := schedulerapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedulerapi.Plugins{
			QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
			PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
			Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
			Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
		},
		PluginConfig: nodeResourcesFitPluginConfig(),
	}

	comps, err := NewFrameworkComponents(ctx, client, informerFactory, WithProfiles(prof), WithParallelism(1))
	if err != nil {
		t.Fatalf("NewFrameworkComponents failed: %v", err)
	}
	snap := internalcache.NewSnapshot(nil, nodes)
	profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, snap)
	if err != nil {
		t.Fatalf("NewFrameworkMap failed: %v", err)
	}
	fwkInst, err := profileMap.FrameworkForPod(&v1.Pod{})
	if err != nil {
		t.Fatalf("FrameworkForPod failed: %v", err)
	}
	fwkInst.SetPodNominator(testNoopPodNominator{})

	allNodeInfos, err := snap.ListNodesInPlacement()
	if err != nil {
		t.Fatalf("ListNodesInPlacement failed: %v", err)
	}
	pod := st.MakePod().Name("pod-1").Namespace("default").UID("uid-1").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "1",
	}).Obj()

	for _, numNodesToFind := range []int32{1, 4, totalNodes} {
		t.Run(fmt.Sprintf("numNodesToFind=%d", numNodesToFind), func(t *testing.T) {
			state := framework.NewCycleState()
			if _, s, _ := fwkInst.RunPreFilterPlugins(ctx, state, pod); !s.IsSuccess() {
				t.Fatalf("RunPreFilterPlugins failed: %v", s)
			}
			diagnosis := framework.Diagnosis{
				NodeToStatus: framework.NewDefaultNodeToStatus(),
			}
			sched := NewScheduler(snap, 0, 0, nil)

			feasible, err := sched.findNodesThatPassFilters(ctx, fwkInst, state, pod, &diagnosis, allNodeInfos, numNodesToFind)
			if err != nil {
				t.Fatalf("findNodesThatPassFilters failed: %v", err)
			}
			if int32(len(feasible)) != numNodesToFind {
				t.Fatalf("len(feasible) = %d, want %d", len(feasible), numNodesToFind)
			}

			gotNames := make([]string, len(feasible))
			wantNames := make([]string, numNodesToFind)
			for i := range feasible {
				gotNames[i] = feasible[i].Node().Name
				wantNames[i] = allNodeInfos[i].Node().Name
			}
			if diff := cmp.Diff(wantNames, gotNames); diff != "" {
				t.Errorf("unexpected feasible nodes (-want +got):\n%s", diff)
			}
		})
	}
}

// countingPlugin lets every node pass Filter and counts the nodes it scores.
type countingPlugin struct {
	scored atomic.Int32
}

var (
	_ fwk.FilterPlugin = &countingPlugin{}
	_ fwk.ScorePlugin  = &countingPlugin{}
)

func (p *countingPlugin) Name() string { return "Counting" }

func (p *countingPlugin) Filter(context.Context, fwk.CycleState, *v1.Pod, fwk.NodeInfo) *fwk.Status {
	return nil
}

func (p *countingPlugin) Score(context.Context, fwk.CycleState, *v1.Pod, fwk.NodeInfo) (int64, *fwk.Status) {
	p.scored.Add(1)
	return 0, nil
}

func (p *countingPlugin) ScoreExtensions() fwk.ScoreExtensions { return nil }

// TestSchedulePodFeasibleNodes checks how many feasible nodes a scheduling attempt collects and
// scores across PercentageOfNodesToScore and WithFirstFit options. Filter runs with a parallelism
// of 1, so it checks the nodes in snapshot order and the winner of a first fit attempt is known.
func TestSchedulePodFeasibleNodes(t *testing.T) {
	// tf.NewFramework, unlike NewFrameworkComponents, does not initialize the scheduler metrics.
	InitMetricsOnce()
	const totalNodes = 300
	nodes := make([]*v1.Node, totalNodes)
	for i := range totalNodes {
		nodes[i] = st.MakeNode().Name(fmt.Sprintf("node-%03d", i)).Obj()
	}
	pod := st.MakePod().Name("pod-1").Namespace("default").UID("uid-1").Obj()
	snap := internalcache.NewSnapshot(nil, nodes)
	// The snapshot lists the nodes in random order, so the expected winners are taken from it.
	allNodes, err := snap.ListNodesInPlacement()
	if err != nil {
		t.Fatalf("ListNodesInPlacement failed: %v", err)
	}
	firstNode, secondNode := allNodes[0].Node().Name, allNodes[1].Node().Name

	tests := []struct {
		name              string
		profilePercentage *int32
		opts              []AlgorithmOption
		extenders         []fwk.Extender
		nominatedNodeName string
		findAll           bool
		wantFeasibleNodes int
		wantScoredNodes   int32
		wantSuggestedHost string
	}{
		{
			name: "adaptive percentage by default",
			// Adaptive percentage for 300 nodes: 50 - 300/125 = 48%, so 300 * 48 / 100 = 144.
			wantFeasibleNodes: 144,
			wantScoredNodes:   144,
		},
		{
			name: "global percentage",
			opts: []AlgorithmOption{WithAlgorithmPercentageOfNodesToScore(10)},
			// 10% of 300 is 30, raised to minFeasibleNodesToFind (100).
			wantFeasibleNodes: 100,
			wantScoredNodes:   100,
		},
		{
			name:              "global 100% evaluates every node",
			opts:              []AlgorithmOption{WithAlgorithmPercentageOfNodesToScore(100)},
			wantFeasibleNodes: totalNodes,
			wantScoredNodes:   totalNodes,
		},
		{
			name:              "profile percentage overrides the global one",
			profilePercentage: ptr.To[int32](100),
			opts:              []AlgorithmOption{WithAlgorithmPercentageOfNodesToScore(10)},
			wantFeasibleNodes: totalNodes,
			wantScoredNodes:   totalNodes,
		},
		{
			name:              "first fit stops at the first feasible node and skips scoring",
			profilePercentage: ptr.To[int32](100),
			opts:              []AlgorithmOption{WithFirstFit(), WithAlgorithmPercentageOfNodesToScore(100)},
			wantFeasibleNodes: 1,
			wantScoredNodes:   0,
			wantSuggestedHost: firstNode,
		},
		{
			name: "first fit with filter extender rejecting first candidate still schedules on another node and skips scoring",
			opts: []AlgorithmOption{WithFirstFit()},
			extenders: []fwk.Extender{
				&tf.FakeExtender{
					ExtenderName: "RejectFirstNodeExtender",
					Predicates: []tf.FitPredicate{
						func(_ *v1.Pod, node fwk.NodeInfo) *fwk.Status {
							if node.Node().Name == firstNode {
								return fwk.NewStatus(fwk.Unschedulable, "rejected by extender")
							}
							return fwk.NewStatus(fwk.Success)
						},
					},
				},
			},
			// Filter collects 144 candidates (adaptive percentage for 300 nodes);
			// the extender rejects the first one, leaving 143 feasible nodes without scoring.
			wantFeasibleNodes: 143,
			wantScoredNodes:   0,
			wantSuggestedHost: secondNode,
		},
		{
			name:              "FindAllNodesThatFitPod ignores first fit",
			opts:              []AlgorithmOption{WithFirstFit()},
			findAll:           true,
			wantFeasibleNodes: totalNodes,
			wantScoredNodes:   0,
		},
		{
			name:              "nominated node is evaluated first and skips scoring",
			nominatedNodeName: secondNode,
			wantFeasibleNodes: 1,
			wantScoredNodes:   0,
			wantSuggestedHost: secondNode,
		},
		{
			name:              "FindAllNodesThatFitPod ignores the nominated node",
			nominatedNodeName: secondNode,
			findAll:           true,
			wantFeasibleNodes: totalNodes,
			wantScoredNodes:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			counter := &countingPlugin{}
			registerPlugins := []tf.RegisterPluginFunc{
				tf.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				tf.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				tf.RegisterPluginAsExtensions(counter.Name(), func(context.Context, runtime.Object, fwk.Handle) (fwk.Plugin, error) {
					return counter, nil
				}, "Filter", "Score"),
				func(_ *frameworkruntime.Registry, profile *schedulerapi.KubeSchedulerProfile) {
					profile.PercentageOfNodesToScore = tt.profilePercentage
				},
			}
			fwkInst, err := tf.NewFramework(ctx, registerPlugins, v1.DefaultSchedulerName,
				frameworkruntime.WithSnapshotSharedLister(snap),
				frameworkruntime.WithPodNominator(testNoopPodNominator{}),
				frameworkruntime.WithExtenders(tt.extenders),
				frameworkruntime.WithParallelism(1),
			)
			if err != nil {
				t.Fatalf("Failed to create framework: %v", err)
			}
			testPod := pod.DeepCopy()
			testPod.Status.NominatedNodeName = tt.nominatedNodeName
			podInfo, err := framework.NewPodInfo(testPod)
			if err != nil {
				t.Fatalf("NewPodInfo failed: %v", err)
			}
			pendingPod := &PendingPod{PodInfo: podInfo, CycleState: framework.NewCycleState()}
			sched := NewScheduler(snap, 0, 0, nil, tt.opts...)

			var gotFeasibleNodes int
			var gotSuggestedHost string
			if tt.findAll {
				feasible, _, err := sched.FindAllNodesThatFitPod(ctx, fwkInst, pendingPod)
				if err != nil {
					t.Fatalf("FindAllNodesThatFitPod failed: %v", err)
				}
				gotFeasibleNodes = len(feasible)
			} else {
				result, err := sched.schedulePod(ctx, fwkInst, pendingPod)
				if err != nil {
					t.Fatalf("schedulePod failed: %v", err)
				}
				gotFeasibleNodes, gotSuggestedHost = result.FeasibleNodes, result.SuggestedHost
			}
			if gotFeasibleNodes != tt.wantFeasibleNodes {
				t.Errorf("feasible nodes = %d, want %d", gotFeasibleNodes, tt.wantFeasibleNodes)
			}
			if got := counter.scored.Load(); got != tt.wantScoredNodes {
				t.Errorf("scored nodes = %d, want %d", got, tt.wantScoredNodes)
			}
			if tt.wantSuggestedHost != "" && gotSuggestedHost != tt.wantSuggestedHost {
				t.Errorf("SuggestedHost = %q, want %q", gotSuggestedHost, tt.wantSuggestedHost)
			}
		})
	}
}

// TestFindNodesThatFitPodWithoutCandidateNodes covers a PreFilter result naming only nodes that are
// not in the snapshot, on a profile without Filter plugins. No budget, not even first fit's single
// node, may make the search index into the empty candidate list.
func TestFindNodesThatFitPodWithoutCandidateNodes(t *testing.T) {
	ctx := t.Context()
	nodes := []*v1.Node{st.MakeNode().Name("node-0").Obj(), st.MakeNode().Name("node-1").Obj()}
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	prof := schedulerapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedulerapi.Plugins{
			QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
			PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeAffinity"}}},
			Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
		},
		PluginConfig: []schedulerapi.PluginConfig{{Name: "NodeAffinity", Args: &schedulerapi.NodeAffinityArgs{}}},
	}
	comps, err := NewFrameworkComponents(ctx, client, informerFactory, WithProfiles(prof))
	if err != nil {
		t.Fatalf("NewFrameworkComponents failed: %v", err)
	}
	snap := internalcache.NewSnapshot(nil, nodes)
	profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, snap)
	if err != nil {
		t.Fatalf("NewFrameworkMap failed: %v", err)
	}
	fwkInst, err := profileMap.FrameworkForPod(&v1.Pod{})
	if err != nil {
		t.Fatalf("FrameworkForPod failed: %v", err)
	}
	fwkInst.SetPodNominator(testNoopPodNominator{})
	// NodeAffinity's PreFilter narrows the candidates to the one node the pod may run on, which
	// does not exist.
	pod := st.MakePod().Name("pod-1").Namespace("default").UID("uid-1").
		NodeAffinityIn(metav1.ObjectNameField, []string{"missing-node"}, st.NodeSelectorTypeMatchFields).Obj()

	for _, tc := range []struct {
		name    string
		opts    []AlgorithmOption
		findAll bool
	}{
		{name: "percentage of nodes to score"},
		{name: "first fit", opts: []AlgorithmOption{WithFirstFit()}},
		{name: "FindAllNodesThatFitPod", findAll: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podInfo, err := framework.NewPodInfo(pod)
			if err != nil {
				t.Fatalf("NewPodInfo failed: %v", err)
			}
			pendingPod := &PendingPod{PodInfo: podInfo, CycleState: framework.NewCycleState()}
			sched := NewScheduler(snap, 0, 0, nil, tc.opts...)
			if tc.findAll {
				feasible, _, err := sched.FindAllNodesThatFitPod(ctx, fwkInst, pendingPod)
				if err != nil {
					t.Fatalf("FindAllNodesThatFitPod failed: %v", err)
				}
				if len(feasible) != 0 {
					t.Errorf("FindAllNodesThatFitPod returned %d nodes, want none", len(feasible))
				}
				return
			}
			_, err = sched.schedulePod(ctx, fwkInst, pendingPod)
			if _, ok := errors.AsType[*framework.FitError](err); !ok {
				t.Errorf("schedulePod error = %v, want a FitError", err)
			}
		})
	}

	t.Run("findNodesThatPassFilters without nodes", func(t *testing.T) {
		diagnosis := framework.Diagnosis{NodeToStatus: framework.NewDefaultNodeToStatus()}
		feasible, err := NewScheduler(snap, 0, 0, nil).findNodesThatPassFilters(ctx, fwkInst, framework.NewCycleState(), pod, &diagnosis, nil, 1)
		if err != nil {
			t.Fatalf("findNodesThatPassFilters failed: %v", err)
		}
		if len(feasible) != 0 {
			t.Errorf("findNodesThatPassFilters returned %d nodes, want none", len(feasible))
		}
	})
}
