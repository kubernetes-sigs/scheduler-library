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
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	"sigs.k8s.io/scheduler-library/pkg/simulator"
	"sigs.k8s.io/scheduler-library/pkg/state"
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
	cfg := newKubeSchedulerConfig()

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

	res, err := snap.CanSchedulePod(ctx, simPod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed: %v", err)
	}
	feasibleNodes, diag := res.FeasibleNodeNames, res.Diagnosis
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

	res, err = snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	feasibleNodes = res.FeasibleNodeNames
	if len(feasibleNodes) != 0 {
		t.Errorf("Expected hugePod to not fit on node1 due to CPU capacity, but got feasible nodes: %v", feasibleNodes)
	}

	// 6. SyncSnapshot should revert snapshot mutations (simPod scheduling)
	err = cs.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}

	// After SyncSnapshot reverts mutations, hugePod should fit on node1 again (2 existing CPU + 6 = 8 <= 10 CPU capacity)
	res, err = snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod after SyncSnapshot: %v", err)
	}
	feasibleNodes, diag = res.FeasibleNodeNames, res.Diagnosis
	if len(feasibleNodes) != 1 || feasibleNodes[0] != "node1" {
		t.Errorf("Expected hugePod to fit on node1 after SyncSnapshot reverted mutations, got feasible nodes %v, diagnosis: %v", feasibleNodes, diag)
	}
}

func TestSimulatorIntegrationFlowWithAssumingPodsInCache(t *testing.T) {
	testCtx := testutils.InitTestAPIServer(t, "simulator-test", nil)
	cs := setupClusterStateWithExistingPod(t, testCtx)
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	snap := cs.GetAssociatedSnapshot()

	// 6. Simulate adding a pod requesting 4 CPU (2 existing + 4 new = 6 <= 10 CPU capacity)
	simPod := st.MakePod().Name("sim-pod").Namespace(testCtx.NS.Name).UID("uid-sim-pod").Req(map[v1.ResourceName]string{
		v1.ResourceCPU: "4",
	}).Obj()

	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

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

	res, err := snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	feasibleNodes := res.FeasibleNodeNames
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
	res, err = snap.CanSchedulePod(ctx, hugePod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for hugePod: %v", err)
	}
	feasibleNodes, diag := res.FeasibleNodeNames, res.Diagnosis
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

func TestSimulatorIntegrationFlowWithReservingFeasibleNode(t *testing.T) {
	testCtx := testutils.InitTestAPIServer(t, "simulator-test", nil)
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet

	// node1 has a single device, so only one pod claiming a device can be reserved on it.
	deviceClass := &resourceapi.DeviceClass{ObjectMeta: metav1.ObjectMeta{Name: "test-class"}}
	if _, err := client.ResourceV1().DeviceClasses().Create(ctx, deviceClass, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create device class in API server: %v", err)
	}
	slice := st.MakeResourceSlice("node1", "test-driver.cdi.k8s.io").Device("instance-1").Obj()
	if _, err := client.ResourceV1().ResourceSlices().Create(ctx, slice, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create resource slice in API server: %v", err)
	}
	makePodWithClaim := func(podName string) *v1.Pod {
		claim := st.MakeResourceClaim().Name(podName + "-claim").Namespace(testCtx.NS.Name).Request(deviceClass.Name).Obj()
		if _, err := client.ResourceV1().ResourceClaims(testCtx.NS.Name).Create(ctx, claim, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create resource claim in API server: %v", err)
		}
		pod := st.MakePod().Name(podName).Namespace(testCtx.NS.Name).UID("uid-" + podName).
			PodResourceClaims(v1.PodResourceClaim{Name: "device", ResourceClaimName: &claim.Name}).
			Obj()
		pod.Spec.Containers = []v1.Container{{
			Name:      "c1",
			Resources: v1.ResourceRequirements{Claims: []v1.ResourceClaim{{Name: "device"}}},
		}}
		return pod
	}
	simPod := makePodWithClaim("sim-pod")
	otherPod := makePodWithClaim("other-pod")

	cs := setupClusterStateWithExistingPod(t, testCtx, withDynamicResources)
	snap := cs.GetAssociatedSnapshot()

	// 6. Check that a pod claiming the device fits
	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	res, err := snap.CanSchedulePod(ctx, simPod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed: %v", err)
	}
	if len(res.FeasibleNodeNames) != 1 || res.FeasibleNodeNames[0] != "node1" {
		t.Fatalf("Expected node1 to be feasible, got nodes %v, diagnosis: %v", res.FeasibleNodeNames, res.Diagnosis)
	}
	if res.CycleState == nil {
		t.Fatal("Expected CanSchedulePod to return a cycle state, got nil")
	}
	if res.Diagnosis == nil || res.Diagnosis.UnschedulablePlugins.Len() != 0 {
		t.Errorf("Expected no unschedulable plugins, got diagnosis: %+v", res.Diagnosis)
	}

	// 7. CanSchedulePod left the snapshot untouched, so another pod claiming the device still fits
	otherRes, err := snap.CanSchedulePod(ctx, otherPod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for otherPod: %v", err)
	}
	if len(otherRes.FeasibleNodeNames) != 1 || otherRes.FeasibleNodeNames[0] != "node1" {
		t.Fatalf("Expected node1 to be feasible for otherPod, got nodes %v, diagnosis: %v", otherRes.FeasibleNodeNames, otherRes.Diagnosis)
	}

	// 8. Assume and reserve the pod on the feasible node with the cycle state CanSchedulePod returned,
	// which holds the device that DynamicResources picked for the pod on that node
	if err := cs.AssumeAndReserve(ctx, simPod, res.CycleState, res.FeasibleNodeNames[0]); err != nil {
		t.Fatalf("failed to assume pod: %v", err)
	}

	// 9. Sync snapshot and check the reservation holds, by trying to schedule the other pod claiming the device
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("Snapshot failed: %v", err)
	}
	snap = cs.GetAssociatedSnapshot()
	placement, err = snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	res, err = snap.CanSchedulePod(ctx, otherPod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed for otherPod: %v", err)
	}
	if len(res.FeasibleNodeNames) != 0 {
		t.Errorf("Expected otherPod to not fit on node1 because the device is reserved, but got feasible nodes: %v", res.FeasibleNodeNames)
	}
	if res.CycleState == nil {
		t.Error("Expected CanSchedulePod to return a cycle state for otherPod, got nil")
	}
	if res.Diagnosis == nil {
		t.Fatal("Expected a diagnosis for otherPod, got nil")
	}
	if !res.Diagnosis.UnschedulablePlugins.Has(dynamicresources.Name) {
		t.Errorf("Expected %s in unschedulable plugins, got %v", dynamicresources.Name, res.Diagnosis.UnschedulablePlugins)
	}
	if status := res.Diagnosis.NodeToStatus.Get("node1"); !strings.Contains(status.Message(), "cannot allocate all claims") {
		t.Errorf("Expected node1 to be rejected because its device is reserved, got status: %v", status)
	}
}

// setupClusterStateWithExistingPod creates node1 with 10 CPU and a pod using 2 CPU on it, in the
// API server of testCtx and in a new ClusterState, and syncs the snapshot. The opts enable
// additional scheduler plugins.
func setupClusterStateWithExistingPod(t *testing.T, testCtx *testutils.TestContext, opts ...configOpt) *state.ClusterState {
	t.Helper()
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
	cfg := newKubeSchedulerConfig(opts...)

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

	return cs
}
