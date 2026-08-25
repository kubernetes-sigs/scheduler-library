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

package scheduler

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	"sigs.k8s.io/scheduler-library/pkg/simulator"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func TestSimulatorIntegration_PodGroupScheduling(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload:                 true,
		features.TopologyAwareWorkloadScheduling: true,
		features.CompositePodGroup:               true,
	})

	testCtx := testutils.InitTestAPIServer(t, "sim-podgroup-test", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet
	ns := testCtx.NS.Name

	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort:         schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					PreFilter:         schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Filter:            schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					PlacementGenerate: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "TopologyPlacementGenerator"}}},
					Bind:              schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
				},
				PluginConfig: []schedulerapi.PluginConfig{
					{
						Name: "NodeResourcesFit",
						Args: &schedulerapi.NodeResourcesFitArgs{
							ScoringStrategy: &schedulerapi.ScoringStrategy{
								Type: schedulerapi.LeastAllocated,
							},
						},
					},
				},
			},
		},
	}

	readonlyClient, err := simulator.NewReadonlyClient(testCtx.KubeConfig)
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()
	tasNode1 := st.MakeNode().Name("node1").Label("topology.kubernetes.io/zone", "zone-a").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()
	tasNode2 := st.MakeNode().Name("node2").Label("topology.kubernetes.io/zone", "zone-b").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "2", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()
	tasNode3 := st.MakeNode().Name("node3").Label("topology.kubernetes.io/zone", "zone-a").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()
	tasNode4 := st.MakeNode().Name("node4").Label("topology.kubernetes.io/zone", "zone-b").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourceMemory: "4Gi", v1.ResourcePods: "10"}).Obj()

	gangPG := st.MakePodGroup().Name("test-gang").Namespace(ns).MinCount(2).Obj()
	rootCPG := st.MakeCompositePodGroup().Name("root-cpg").Namespace(ns).MinGroupCount(2).Obj()
	leafPG1 := st.MakePodGroup().Name("leaf-pg-1").Namespace(ns).ParentCompositePodGroup("root-cpg").Obj()
	leafPG2 := st.MakePodGroup().Name("leaf-pg-2").Namespace(ns).ParentCompositePodGroup("root-cpg").Obj()
	failRootCPG := st.MakeCompositePodGroup().Name("root-fail-cpg").Namespace(ns).MinGroupCount(2).Obj()
	failLeafPG1 := st.MakePodGroup().Name("leaf-fail-1").Namespace(ns).ParentCompositePodGroup("root-fail-cpg").Obj()
	failLeafPG2 := st.MakePodGroup().Name("leaf-fail-2").Namespace(ns).ParentCompositePodGroup("root-fail-cpg").Obj()
	tasPG := st.MakePodGroup().Name("tas-gang-pg").Namespace(ns).TopologyKey("topology.kubernetes.io/zone").MinCount(2).Obj()
	rootTASCPG := st.MakeCompositePodGroup().Name("root-tas-cpg").Namespace(ns).TopologyKey("topology.kubernetes.io/zone").MinGroupCount(2).Obj()
	leafTASPG1 := st.MakePodGroup().Name("leaf-tas-1").Namespace(ns).ParentCompositePodGroup("root-tas-cpg").Obj()
	leafTASPG2 := st.MakePodGroup().Name("leaf-tas-2").Namespace(ns).ParentCompositePodGroup("root-tas-cpg").Obj()

	gangPod1 := st.MakePod().Name("pod-1").Namespace(ns).PodGroupName("test-gang").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	gangPod2 := st.MakePod().Name("pod-2").Namespace(ns).PodGroupName("test-gang").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	cpgPod1 := st.MakePod().Name("cpg-pod-1").Namespace(ns).PodGroupName("leaf-pg-1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	cpgPod2 := st.MakePod().Name("cpg-pod-2").Namespace(ns).PodGroupName("leaf-pg-2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	failPod1 := st.MakePod().Name("fail-pod-1").Namespace(ns).PodGroupName("leaf-fail-1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "3"}).Obj()
	failPod2 := st.MakePod().Name("fail-pod-2").Namespace(ns).PodGroupName("leaf-fail-2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "3"}).Obj()
	tasPod1 := st.MakePod().Name("tas-pod-1").Namespace(ns).PodGroupName("tas-gang-pg").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	tasPod2 := st.MakePod().Name("tas-pod-2").Namespace(ns).PodGroupName("tas-gang-pg").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	cpgTASPod1 := st.MakePod().Name("cpg-tas-pod-1").Namespace(ns).PodGroupName("leaf-tas-1").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	cpgTASPod2 := st.MakePod().Name("cpg-tas-pod-2").Namespace(ns).PodGroupName("leaf-tas-2").Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()

	tests := []struct {
		name               string
		nodes              []*v1.Node
		podGroups          []*schedulingv1beta1.PodGroup
		compositePodGroups []*schedulingv1alpha3.CompositePodGroup
		pods               []*v1.Pod
		wantSuccess        bool
		wantResultsCount   int
	}{
		{
			name:             "single pod group hierarchy",
			nodes:            []*v1.Node{node1, node2},
			podGroups:        []*schedulingv1beta1.PodGroup{gangPG},
			pods:             []*v1.Pod{gangPod1, gangPod2},
			wantSuccess:      true,
			wantResultsCount: 2,
		},
		{
			name:               "multi-level hierarchy",
			nodes:              []*v1.Node{node1},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG},
			podGroups:          []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			pods:               []*v1.Pod{cpgPod1, cpgPod2},
			wantSuccess:        true,
			wantResultsCount:   2,
		},
		{
			name:               "multi-level hierarchy scheduling failure due to insufficient capacity and reverting results",
			nodes:              []*v1.Node{node1},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{failRootCPG},
			podGroups:          []*schedulingv1beta1.PodGroup{failLeafPG1, failLeafPG2},
			pods:               []*v1.Pod{failPod1, failPod2},
			wantSuccess:        false,
			wantResultsCount:   2,
		},
		{
			name:             "single pod group hierarchy with TAS fails due to insufficient zone capacity",
			nodes:            []*v1.Node{tasNode1, tasNode2},
			podGroups:        []*schedulingv1beta1.PodGroup{tasPG},
			pods:             []*v1.Pod{tasPod1, tasPod2},
			wantSuccess:      false,
			wantResultsCount: 2,
		},
		{
			name:               "multi-level hierarchy TAS success with sufficient zone capacity",
			nodes:              []*v1.Node{tasNode3, tasNode4},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootTASCPG},
			podGroups:          []*schedulingv1beta1.PodGroup{leafTASPG1, leafTASPG2},
			pods:               []*v1.Pod{cpgTASPod1, cpgTASPod2},
			wantSuccess:        true,
			wantResultsCount:   2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			sim, err := simulator.NewSchedulingSimulator(ctx, cfg, readonlyClient, informerFactory)
			if err != nil {
				t.Fatalf("NewSchedulingSimulator failed: %v", err)
			}

			cs, err := sim.NewClusterState(ctx)
			if err != nil {
				t.Fatalf("NewClusterState failed: %v", err)
			}

			for _, n := range tt.nodes {
				cs.Cache.AddNode(logger, n)
			}
			for _, cpg := range tt.compositePodGroups {
				cs.Cache.AddCompositePodGroup(logger, cpg)
			}
			for _, pg := range tt.podGroups {
				cs.Cache.AddPodGroup(pg)
			}

			snap := cs.GetAssociatedSnapshot()
			if err := cs.SyncSnapshot(logger); err != nil {
				t.Fatalf("SyncSnapshot failed: %v", err)
			}

			res, err := snap.ScheduleWorkload(ctx, tt.pods, snapshot.NewScheduleWorkloadOptions(false))
			if err != nil {
				t.Fatalf("ScheduleWorkload failed: %v", err)
			}
			if len(res) != tt.wantResultsCount {
				t.Fatalf("ScheduleWorkload returned %d results, want %d", len(res), tt.wantResultsCount)
			}

			for _, r := range res {
				if r.Status.IsSuccess() != tt.wantSuccess {
					t.Errorf("pod %s Status.IsSuccess() = %v, want %v (status: %v)", r.Pod.Name, r.Status.IsSuccess(), tt.wantSuccess, r.Status)
				}
			}
		})
	}
}
