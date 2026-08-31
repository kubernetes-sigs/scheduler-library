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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	"sigs.k8s.io/scheduler-library/pkg/simulator"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func TestSimulatorIntegrationFlow(t *testing.T) {
	testCtx := testutils.InitTestAPIServer(t, "simulator-test", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet

	// 1. Create a node and an initial pod in the API server
	node := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	if _, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create node in API server: %v", err)
	}

	existingPod := st.MakePod().Name("existing-pod").Namespace(testCtx.NS.Name).UID("uid-existing-pod").Node("node1").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "2",
	}).Obj()
	if _, err := client.CoreV1().Pods(testCtx.NS.Name).Create(ctx, existingPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create existing pod in API server: %v", err)
	}

	// 2. Initialize factory
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	// 3. Create the SchedulingSimulator
	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
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
	sim, err := simulator.NewSchedulingSimulator(ctx, cfg, readonlyClient, informerFactory)
	if err != nil {
		t.Fatalf("NewSchedulingSimulator failed: %v", err)
	}

	// 4. Create ClusterState
	cs, err := sim.NewClusterState(ctx)
	if err != nil {
		t.Fatalf("NewClusterState failed: %v", err)
	}

	cs.Cache.AddNode(logger, node)

	if err := cs.Cache.AddPod(logger, existingPod); err != nil {
		t.Fatalf("failed to add pod to cluster state cache: %v", err)
	}

	// 5. Take snapshot and run simulations
	snap := cs.GetAssociatedSnapshot()
	err = cs.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}

	// Simulate a pod requesting 4 CPU (2 existing + 4 new = 6 <= 10 CPU capacity)
	simPod := st.MakePod().Name("sim-pod").Namespace(testCtx.NS.Name).UID("uid-sim-pod").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}).Obj()

	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	feasibleNodes, diag, err := snap.CanSchedulePod(ctx, simPod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed: %v", err)
	}
	if len(feasibleNodes) != 1 || feasibleNodes[0] != "node1" {
		t.Fatalf("Expected node1 to be feasible, got nodes %v, diagnosis: %v", feasibleNodes, diag)
	}

	// Schedule the pod on the snapshot
	results, err := snap.SchedulePods(ctx, []*v1.Pod{simPod}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	if len(results) != 1 || !results[0].Status.IsSuccess() {
		t.Fatalf("Expected scheduling success, got results: %+v", results)
	}
	if results[0].SelectedNodeName != "node1" {
		t.Errorf("Expected pod to be scheduled on node1, got %q", results[0].SelectedNodeName)
	}

	// Now try simulating a pod that requires 6 CPU (2 existing + 4 scheduled + 6 = 12 > 10 capacity)
	hugePod := st.MakePod().Name("huge-pod").Namespace(testCtx.NS.Name).UID("uid-huge-pod").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "6",
	}).Obj()

	feasibleNodes, _, err = snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	if len(feasibleNodes) != 0 {
		t.Errorf("Expected hugePod to not fit on node1 due to CPU capacity, but got feasible nodes: %v", feasibleNodes)
	}

	// 6. SyncSnapshot should revert snapshot mutations (simPod scheduling)
	err = cs.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}

	// After SyncSnapshot reverts mutations, hugePod should fit on node1 again (2 existing CPU + 6 = 8 <= 10 CPU capacity)
	feasibleNodes, diag, err = snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod after SyncSnapshot: %v", err)
	}
	if len(feasibleNodes) != 1 || feasibleNodes[0] != "node1" {
		t.Errorf("Expected hugePod to fit on node1 after SyncSnapshot reverted mutations, got feasible nodes %v, diagnosis: %v", feasibleNodes, diag)
	}
}

func TestSimulatorIntegrationFlowWithAssumingPodsInCache(t *testing.T) {
	testCtx := testutils.InitTestAPIServer(t, "simulator-test", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet

	// 1. Create a node and an initial pod in the API server
	node := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	if _, err := client.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create node in API server: %v", err)
	}

	existingPod := st.MakePod().Name("existing-pod").Namespace(testCtx.NS.Name).UID("uid-existing-pod").Node("node1").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "2",
	}).Obj()
	if _, err := client.CoreV1().Pods(testCtx.NS.Name).Create(ctx, existingPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create existing pod in API server: %v", err)
	}

	// 2. Initialize factory
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	// 3. Create the SchedulingSimulator
	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
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
	sim, err := simulator.NewSchedulingSimulator(ctx, cfg, readonlyClient, informerFactory)
	if err != nil {
		t.Fatalf("NewSchedulingSimulator failed: %v", err)
	}

	// 4. Create ClusterState
	cs, err := sim.NewClusterState(ctx)
	if err != nil {
		t.Fatalf("NewClusterState failed: %v", err)
	}

	cs.Cache.AddNode(logger, node)

	if err := cs.Cache.AddPod(logger, existingPod); err != nil {
		t.Fatalf("failed to add pod to cluster state cache: %v", err)
	}

	// 5. Take snapshot
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	snap := cs.GetAssociatedSnapshot()
	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	// 6. Simulate adding a pod requesting 4 CPU (2 existing + 4 new = 6 <= 10 CPU capacity)
	simPod := st.MakePod().Name("sim-pod").Namespace(testCtx.NS.Name).UID("uid-sim-pod").Node("node1").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}).Obj()

	results, err := snap.SchedulePods(ctx, []*v1.Pod{simPod}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	if len(results) != 1 || !results[0].Status.IsSuccess() {
		t.Fatalf("Expected scheduling success, got results: %+v", results)
	}
	if results[0].SelectedNodeName != "node1" {
		t.Errorf("Expected pod to be scheduled on node1, got %q", results[0].SelectedNodeName)
	}
	cycleState := results[0].CycleState
	nodeName := results[0].SelectedNodeName
	// 7. Assume and reserve simulated pod requesting 4 CPU (2 existing + 4 new = 6 <= 10 CPU capacity)
	if err := cs.AssumeAndReserve(ctx, simPod, cycleState, nodeName); err != nil {
		t.Fatalf("failed to assume pod: %v", err)
	}

	// 8. Take snapshot and run simulations
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	snap = cs.GetAssociatedSnapshot()

	// 9. Now try simulating a pod that requires 6 CPU (2 existing + 4 scheduled + 6 = 12 > 10 capacity)
	hugePod := st.MakePod().Name("huge-pod").Namespace(testCtx.NS.Name).UID("uid-huge-pod").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "6",
	}).Obj()

	feasibleNodes, _, err := snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	if len(feasibleNodes) != 0 {
		t.Errorf("Expected hugePod to not fit on node1 due to CPU capacity, but got feasible nodes: %v", feasibleNodes)
	}

	// 10. Release the reservation (4 CPU)
	if err := cs.UnreserveAndForget(ctx, simPod); err != nil {
		t.Fatalf("failed to unreserve and forget pod: %v", err)
	}

	// 11. Take new snapshot
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	placement, err = snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	// 12. Check if huge pod fits on node1 (2 existing + 6 new = 8 <= 10 CPU capacity)
	feasibleNodes, diag, err := snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	if len(feasibleNodes) != 1 || feasibleNodes[0] != "node1" {
		t.Fatalf("Expected node1 to be feasible, got nodes %v, diagnosis: %v", feasibleNodes, diag)
	}

	// 13. Try scheduling huge pod on the snapshot
	results, err = snap.SchedulePods(ctx, []*v1.Pod{hugePod}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	if len(results) != 1 || !results[0].Status.IsSuccess() {
		t.Fatalf("Expected scheduling success, got results: %+v", results)
	}
	if results[0].SelectedNodeName != "node1" {
		t.Errorf("Expected pod to be scheduled on node1, got %q", results[0].SelectedNodeName)
	}
}
