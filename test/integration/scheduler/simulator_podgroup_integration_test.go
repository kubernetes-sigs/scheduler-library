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
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	testutils "k8s.io/kubernetes/test/integration/util"
	"sigs.k8s.io/scheduler-library/pkg/simulator"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

const zoneLabel = "topology.kubernetes.io/zone"

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

	cfg := newKubeSchedulerConfig(withTopologyPlacementGenerator)

	readonlyClient, err := simulator.NewReadonlyClient(testCtx.KubeConfig)
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	tasNode1 := st.MakeNode().Name("node1").Label(zoneLabel, "zone-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	tasNode2 := st.MakeNode().Name("node2").Label(zoneLabel, "zone-b").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	tasNode3 := st.MakeNode().Name("node3").Label(zoneLabel, "zone-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	tasNode4 := st.MakeNode().Name("node4").Label(zoneLabel, "zone-b").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPG := makePodGroup(ns, "test-gang", "", 2, "")
	rootCPG := makeCompositePodGroup(ns, "root-cpg", 2, "")
	leafPG1 := makePodGroup(ns, "leaf-pg-1", "root-cpg", 0, "")
	leafPG2 := makePodGroup(ns, "leaf-pg-2", "root-cpg", 0, "")
	failRootCPG := makeCompositePodGroup(ns, "root-fail-cpg", 2, "")
	failLeafPG1 := makePodGroup(ns, "leaf-fail-1", "root-fail-cpg", 0, "")
	failLeafPG2 := makePodGroup(ns, "leaf-fail-2", "root-fail-cpg", 0, "")
	tasPG := makePodGroup(ns, "tas-gang-pg", "", 2, zoneLabel)
	rootTASCPG := makeCompositePodGroup(ns, "root-tas-cpg", 2, zoneLabel)
	leafTASPG1 := makePodGroup(ns, "leaf-tas-1", "root-tas-cpg", 0, "")
	leafTASPG2 := makePodGroup(ns, "leaf-tas-2", "root-tas-cpg", 0, "")

	gangPod1 := makePod(ns, "pod-1", "test-gang", "2")
	gangPod2 := makePod(ns, "pod-2", "test-gang", "2")
	cpgPod1 := makePod(ns, "cpg-pod-1", "leaf-pg-1", "1")
	cpgPod2 := makePod(ns, "cpg-pod-2", "leaf-pg-2", "1")
	failPod1 := makePod(ns, "fail-pod-1", "leaf-fail-1", "3")
	failPod2 := makePod(ns, "fail-pod-2", "leaf-fail-2", "3")
	tasPod1 := makePod(ns, "tas-pod-1", "tas-gang-pg", "2")
	tasPod2 := makePod(ns, "tas-pod-2", "tas-gang-pg", "2")
	cpgTASPod1 := makePod(ns, "cpg-tas-pod-1", "leaf-tas-1", "2")
	cpgTASPod2 := makePod(ns, "cpg-tas-pod-2", "leaf-tas-2", "2")

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

			res := snap.ScheduleWorkload(ctx, tt.pods, snapshot.NewScheduleWorkloadOptions(false))
			if res.Status.IsError() {
				t.Fatalf("ScheduleWorkload failed: %v", res.Status.AsError())
			}
			if res.Status.IsSuccess() != tt.wantSuccess {
				t.Errorf("Workload Status.IsSuccess() = %v, want %v (status: %v)", res.Status.IsSuccess(), tt.wantSuccess, res.Status)
			}
			if len(res.PodResults) != tt.wantResultsCount {
				t.Fatalf("ScheduleWorkload returned %d results, want %d", len(res.PodResults), tt.wantResultsCount)
			}

			for _, r := range res.PodResults {
				if r.Status.IsSuccess() != tt.wantSuccess {
					t.Errorf("pod %s Status.IsSuccess() = %v, want %v (status: %v)", r.Pod.Name, r.Status.IsSuccess(), tt.wantSuccess, r.Status)
				}
			}
		})
	}
}

func TestSimulatorIntegration_AddAndRemovePodGroups(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload:                 true,
		features.TopologyAwareWorkloadScheduling: true,
		features.CompositePodGroup:               true,
	})

	testCtx := testutils.InitTestAPIServer(t, "sim-pg-snapshot", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet
	ns := testCtx.NS.Name

	cfg := newKubeSchedulerConfig(withTopologyPlacementGenerator)

	readonlyClient, err := simulator.NewReadonlyClient(testCtx.KubeConfig)
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := simulator.NewSchedulingSimulator(ctx, cfg, readonlyClient, informerFactory)
	if err != nil {
		t.Fatalf("NewSchedulingSimulator failed: %v", err)
	}

	cs, err := sim.NewClusterState(ctx)
	if err != nil {
		t.Fatalf("NewClusterState failed: %v", err)
	}

	tasNode1 := st.MakeNode().Name("node1").Label(zoneLabel, "zone-a").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	tasNode2 := st.MakeNode().Name("node2").Label(zoneLabel, "zone-b").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	initialPG := makePodGroup(ns, "initial-gang", "", 2, "")
	initialPod1 := makePod(ns, "initial-pod-1", "initial-gang", "2")
	initialPod2 := makePod(ns, "initial-pod-2", "initial-gang", "2")
	initialPods := []*v1.Pod{initialPod1, initialPod2}

	addedPG := makePodGroup(ns, "added-gang", "", 2, "")
	addedPod1 := makePod(ns, "added-pod-1", "added-gang", "2")
	addedPod2 := makePod(ns, "added-pod-2", "added-gang", "2")
	addedPods := []*v1.Pod{addedPod1, addedPod2}

	rootTASCPG := makeCompositePodGroup(ns, "root-tas-cpg", 2, zoneLabel)
	leafTASPG1 := makePodGroup(ns, "leaf-tas-1", "root-tas-cpg", 0, "")
	leafTASPG2 := makePodGroup(ns, "leaf-tas-2", "root-tas-cpg", 0, "")
	cpgTASPod1 := makePod(ns, "cpg-tas-pod-1", "leaf-tas-1", "2")
	cpgTASPod2 := makePod(ns, "cpg-tas-pod-2", "leaf-tas-2", "2")
	cpgTASPods := []*v1.Pod{cpgTASPod1, cpgTASPod2}

	// 1. Add nodes and initialPG to cluster state cache, then sync snapshot.
	cs.Cache.AddNode(logger, tasNode1)
	cs.Cache.AddNode(logger, tasNode2)
	cs.Cache.AddPodGroup(initialPG)

	snap := cs.GetAssociatedSnapshot()
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}

	assertScheduleWorkloadSuccess := func(pods []*v1.Pod, dryRun bool) {
		t.Helper()
		res := snap.ScheduleWorkload(ctx, pods, snapshot.NewScheduleWorkloadOptions(dryRun))
		if res.Status.IsError() {
			t.Fatalf("ScheduleWorkload unexpected error: %v", res.Status.AsError())
		}
		if len(res.PodResults) != len(pods) {
			t.Fatalf("ScheduleWorkload returned %d results, want %d", len(res.PodResults), len(pods))
		}
		for _, r := range res.PodResults {
			if !r.Status.IsSuccess() {
				t.Errorf("pod %s Status.IsSuccess() = false, want true (status: %v)", r.Pod.Name, r.Status)
			}
		}
	}

	assertScheduleWorkloadError := func(pods []*v1.Pod) {
		t.Helper()
		if res := snap.ScheduleWorkload(ctx, pods, snapshot.NewScheduleWorkloadOptions(false)); !res.Status.IsError() {
			t.Fatalf("Expected ScheduleWorkload to fail when pod group is missing from snapshot, got nil error")
		}
	}

	// 2. Verify initialPG can be scheduled, while addedPG (not yet in snapshot) fails.
	assertScheduleWorkloadSuccess(initialPods, true)
	assertScheduleWorkloadError(addedPods)

	// 3. Remove initialPG from snapshot and verify it can no longer be scheduled.
	if err := snap.RemovePodGroup(ctx, initialPG); err != nil {
		t.Fatalf("RemovePodGroup(initialPG) failed: %v", err)
	}
	assertScheduleWorkloadError(initialPods)

	// 4. Add addedPG to snapshot and verify its workload can now be scheduled.
	if err := snap.AddPodGroup(ctx, addedPG); err != nil {
		t.Fatalf("AddPodGroup(addedPG) failed: %v", err)
	}
	assertScheduleWorkloadSuccess(addedPods, true)

	// 5. Add and remove a multi-level CompositePodGroup hierarchy on snapshot.
	if err := snap.AddCompositePodGroup(ctx, rootTASCPG); err != nil {
		t.Fatalf("AddCompositePodGroup(rootTASCPG) failed: %v", err)
	}
	for _, pg := range []*schedulingv1beta1.PodGroup{leafTASPG1, leafTASPG2} {
		if err := snap.AddPodGroup(ctx, pg); err != nil {
			t.Fatalf("AddPodGroup(%s) failed: %v", pg.Name, err)
		}
	}
	assertScheduleWorkloadSuccess(cpgTASPods, true)

	for _, pg := range []*schedulingv1beta1.PodGroup{leafTASPG1, leafTASPG2} {
		if err := snap.RemovePodGroup(ctx, pg); err != nil {
			t.Fatalf("RemovePodGroup(%s) failed: %v", pg.Name, err)
		}
	}
	if err := snap.RemoveCompositePodGroup(ctx, rootTASCPG); err != nil {
		t.Fatalf("RemoveCompositePodGroup(rootTASCPG) failed: %v", err)
	}
	assertScheduleWorkloadError(cpgTASPods)

	// 6. Verify Add and Remove roll back when a Transaction is reverted.
	if err := snap.Transaction(ctx, func() (snapshot.TransactionResult, error) {
		if err := snap.AddCompositePodGroup(ctx, rootTASCPG); err != nil {
			t.Fatalf("AddCompositePodGroup in transaction failed: %v", err)
		}
		for _, pg := range []*schedulingv1beta1.PodGroup{leafTASPG1, leafTASPG2} {
			if err := snap.AddPodGroup(ctx, pg); err != nil {
				t.Fatalf("AddPodGroup in transaction failed: %v", err)
			}
		}
		if err := snap.RemovePodGroup(ctx, addedPG); err != nil {
			t.Fatalf("RemovePodGroup in transaction failed: %v", err)
		}

		assertScheduleWorkloadSuccess(cpgTASPods, false)
		assertScheduleWorkloadError(addedPods)

		return snapshot.Revert, nil
	}); err != nil {
		t.Fatalf("Transaction failed: %v", err)
	}

	// After transaction revert, hierarchy is gone and addedPG is back.
	assertScheduleWorkloadError(cpgTASPods)
	assertScheduleWorkloadSuccess(addedPods, false)

	// 7. SyncSnapshot resets snapshot mutations back to the ClusterState cache (initialPG present, addedPG absent).
	if err := cs.SyncSnapshot(logger); err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}
	assertScheduleWorkloadError(addedPods)
	assertScheduleWorkloadSuccess(initialPods, false)
}

func makePod(ns, name, podGroupName, cpu string) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(ns).UID(name)
	if podGroupName != "" {
		pod = pod.PodGroupName(podGroupName)
	}
	if cpu != "" {
		pod = pod.Req(map[v1.ResourceName]string{v1.ResourceCPU: cpu})
	}
	return pod.Obj()
}

func makeScheduledPod(ns, name, nodeName, podGroupName, cpu string) *v1.Pod {
	pod := makePod(ns, name, podGroupName, cpu)
	pod.Spec.NodeName = nodeName
	return pod
}

func makePodGroup(ns, name, parentCPG string, minCount int32, topologyKey string) *schedulingv1beta1.PodGroup {
	pg := st.MakePodGroup().Name(name).Namespace(ns)
	if parentCPG != "" {
		pg = pg.ParentCompositePodGroup(parentCPG)
	}
	if minCount > 0 {
		pg = pg.MinCount(minCount)
	}
	if topologyKey != "" {
		pg = pg.TopologyKey(topologyKey)
	}
	return pg.Obj()
}

func makeCompositePodGroup(ns, name string, minGroupCount int32, topologyKey string) *schedulingv1alpha3.CompositePodGroup {
	cpg := st.MakeCompositePodGroup().Name(name).Namespace(ns)
	if minGroupCount > 0 {
		cpg = cpg.MinGroupCount(minGroupCount)
	}
	if topologyKey != "" {
		cpg = cpg.TopologyKey(topologyKey)
	}
	return cpg.Obj()
}

type integrationVictim struct {
	name string
	pods []*v1.Pod
}

func (v *integrationVictim) Pods() []*v1.Pod {
	return v.pods
}

type integrationCPUPreemptionFilter struct {
	availableCPU int64
	requiredCPU  int64
}

func (f *integrationCPUPreemptionFilter) PreemptVictim(v snapshot.PreemptionVictim) {
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.availableCPU += cpu.Value()
		}
	}
}

func (f *integrationCPUPreemptionFilter) UnpreemptVictim(v snapshot.PreemptionVictim) {
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.availableCPU -= cpu.Value()
		}
	}
}

func (f *integrationCPUPreemptionFilter) MayFit() bool {
	return f.availableCPU >= f.requiredCPU
}

func TestSimulatorIntegration_PodGroupPreemption(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload:                 true,
		features.TopologyAwareWorkloadScheduling: true,
		features.CompositePodGroup:               true,
	})

	testCtx := testutils.InitTestAPIServer(t, "sim-pg-preempt-test", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet
	ns := testCtx.NS.Name

	cfg := newKubeSchedulerConfig(withTopologyPlacementGenerator)

	readonlyClient, err := simulator.NewReadonlyClient(testCtx.KubeConfig)
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	node6CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "6",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPG := makePodGroup(ns, "preempt-gang", "", 2, "")

	v1Pod := makeScheduledPod(ns, "v1-pod", "node1", "", "2")
	v2Pod := makeScheduledPod(ns, "v2-pod", "node1", "", "2")
	vSmallPod := makeScheduledPod(ns, "v-small", "node1", "", "1")
	vMedPod := makeScheduledPod(ns, "v-med", "node1", "", "3")
	vHighPod := makeScheduledPod(ns, "v-high", "node1", "", "1")
	vMulti1 := makeScheduledPod(ns, "v-multi-1", "node1", "", "1")
	vMulti2 := makeScheduledPod(ns, "v-multi-2", "node1", "", "1")
	vSingle := makeScheduledPod(ns, "v-single", "node1", "", "4")
	vCommittedPod := makeScheduledPod(ns, "v-committed", "node1", "", "2")

	tests := []struct {
		name                     string
		nodes                    []*v1.Node
		existingPods             []*v1.Pod
		workloadPods             []*v1.Pod
		opts                     snapshot.ScheduleWorkloadOptions
		wantStatusCode           fwk.Code
		wantScheduled            bool
		wantWaitingForPreemption bool
		wantVictimNames          []string
		probePodCPU              string
		wantProbeFits            bool
	}{
		{
			name:         "fits without preemption",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{v1Pod, v2Pod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-a1", "preempt-gang", "2"),
				makePod(ns, "w-a2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "v1", pods: []*v1.Pod{v1Pod}},
						&integrationVictim{name: "v2", pods: []*v1.Pod{v2Pod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{availableCPU: 4, requiredCPU: 4},
				},
			},
			wantStatusCode:           fwk.Success,
			wantScheduled:            true,
			wantWaitingForPreemption: false,
			wantVictimNames:          nil,
			probePodCPU:              "1",
			wantProbeFits:            false,
		},
		{
			name:         "preemption triggered by PreemptionFilter when node has free capacity",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-free1", "preempt-gang", "1"),
				makePod(ns, "w-free2", "preempt-gang", "1"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}},
						&integrationVictim{name: "vMed", pods: []*v1.Pod{vMedPod}},
						&integrationVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{requiredCPU: 3},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"vMed"},
			probePodCPU:              "3",
			wantProbeFits:            true,
		},
		{
			name:         "fits after preempting subset of PotentialVictims with reprieve",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-b1", "preempt-gang", "2"),
				makePod(ns, "w-b2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}},
						&integrationVictim{name: "vMed", pods: []*v1.Pod{vMedPod}},
						&integrationVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{requiredCPU: 3},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"vMed"},
			probePodCPU:              "1",
			wantProbeFits:            true,
		},
		{
			name:         "fits after initial node-fit failure without PreemptionFilter",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-nofilter1", "preempt-gang", "2"),
				makePod(ns, "w-nofilter2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}},
						&integrationVictim{name: "vMed", pods: []*v1.Pod{vMedPod}},
						&integrationVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}},
					}),
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"vMed"},
			probePodCPU:              "1",
			wantProbeFits:            true,
		},
		{
			name:         "multi-pod PreemptionVictim preempted and restored atomically",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vMulti1, vMulti2, vSingle},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-c1", "preempt-gang", "2"),
				makePod(ns, "w-c2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vMulti", pods: []*v1.Pod{vMulti1, vMulti2}},
						&integrationVictim{name: "vSingle", pods: []*v1.Pod{vSingle}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{requiredCPU: 4},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"vSingle"},
			probePodCPU:              "1",
			wantProbeFits:            false,
		},
		{
			name:         "CommittedVictims removed upfront and only new victims included in PreemptionVictims",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{v1Pod, v2Pod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-d1", "preempt-gang", "3"),
				makePod(ns, "w-d2", "preempt-gang", "3"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					CommittedVictims: []snapshot.PreemptionVictim{
						&integrationVictim{name: "vCommitted", pods: []*v1.Pod{vCommittedPod}},
					},
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "v1", pods: []*v1.Pod{v1Pod}},
						&integrationVictim{name: "v2", pods: []*v1.Pod{v2Pod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{availableCPU: 4, requiredCPU: 6},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"v1"},
			probePodCPU:              "2",
			wantProbeFits:            true,
		},
		{
			name:         "fails when all PotentialVictims are insufficient and reverts snapshot",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{v1Pod, v2Pod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-e1", "preempt-gang", "4"),
				makePod(ns, "w-e2", "preempt-gang", "4"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "v1", pods: []*v1.Pod{v1Pod}},
						&integrationVictim{name: "v2", pods: []*v1.Pod{v2Pod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{requiredCPU: 2},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            false,
			wantWaitingForPreemption: false,
			wantVictimNames:          nil,
			probePodCPU:              "2",
			wantProbeFits:            true,
		},
		{
			name:         "DryRun with preemption returns victims and reverts snapshot",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-f1", "preempt-gang", "2"),
				makePod(ns, "w-f2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				CommonSchedulingOptions: snapshot.CommonSchedulingOptions{DryRun: true},
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}},
						&integrationVictim{name: "vMed", pods: []*v1.Pod{vMedPod}},
						&integrationVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{requiredCPU: 3},
				},
			},
			wantStatusCode:           fwk.Unschedulable,
			wantScheduled:            true,
			wantWaitingForPreemption: true,
			wantVictimNames:          []string{"vMed"},
			probePodCPU:              "2",
			wantProbeFits:            false,
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
			for _, p := range tt.existingPods {
				if err := cs.Cache.AddPod(logger, p.DeepCopy()); err != nil {
					t.Fatalf("AddPod(%s) failed: %v", p.Name, err)
				}
			}
			cs.Cache.AddPodGroup(gangPG)

			snap := cs.GetAssociatedSnapshot()
			if err := cs.SyncSnapshot(logger); err != nil {
				t.Fatalf("SyncSnapshot failed: %v", err)
			}

			res := snap.ScheduleWorkload(ctx, tt.workloadPods, tt.opts)
			if res.Status.IsError() {
				t.Fatalf("ScheduleWorkload failed: %v", res.Status.AsError())
			}
			if res.Status.Code() != tt.wantStatusCode {
				t.Fatalf("Status.Code() = %v, want %v (status: %v)", res.Status.Code(), tt.wantStatusCode, res.Status)
			}
			if res.WaitingForPreemption != tt.wantWaitingForPreemption {
				t.Errorf("WaitingForPreemption = %v, want %v", res.WaitingForPreemption, tt.wantWaitingForPreemption)
			}
			if tt.wantScheduled {
				if len(res.PodResults) != len(tt.workloadPods) {
					t.Fatalf("len(PodResults) = %d, want %d", len(res.PodResults), len(tt.workloadPods))
				}
				for _, pRes := range res.PodResults {
					if !pRes.Status.IsSuccess() {
						t.Errorf("pod %s expected success, got %v", pRes.Pod.Name, pRes.Status)
					}
				}
			} else if len(res.PodResults) != 0 {
				t.Errorf("expected empty PodResults on failure, got %v", res.PodResults)
			}

			var gotVictims []string
			for _, v := range res.PreemptionVictims {
				gotVictims = append(gotVictims, v.(*integrationVictim).name)
			}
			if len(gotVictims) != len(tt.wantVictimNames) {
				t.Fatalf("PreemptionVictims = %v, want %v", gotVictims, tt.wantVictimNames)
			}
			for i := range gotVictims {
				if gotVictims[i] != tt.wantVictimNames[i] {
					t.Errorf("PreemptionVictims[%d] = %q, want %q", i, gotVictims[i], tt.wantVictimNames[i])
				}
			}

			placement, err := snap.MakePlacement([]string{"node1"})
			if err != nil {
				t.Fatalf("MakePlacement failed: %v", err)
			}
			probePod := makePod(ns, "probe-pod", "", tt.probePodCPU)
			feasibleNodes, _, err := snap.CanSchedulePod(ctx, probePod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePod failed: %v", err)
			}
			gotFits := len(feasibleNodes) == 1
			if gotFits != tt.wantProbeFits {
				t.Errorf("CanSchedulePod(probe %s CPU) fits = %v, want %v (feasibleNodes: %v)", tt.probePodCPU, gotFits, tt.wantProbeFits, feasibleNodes)
			}
		})
	}
}

func TestSimulatorIntegration_IncrementalPreemption(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload:                 true,
		features.TopologyAwareWorkloadScheduling: true,
		features.CompositePodGroup:               true,
	})

	testCtx := testutils.InitTestAPIServer(t, "sim-inc-preempt-test", nil)
	if testCtx == nil {
		t.Fatal("Expected testCtx to be non-nil")
	}
	ctx := testCtx.Ctx
	logger := klog.FromContext(ctx)
	client := testCtx.ClientSet
	ns := testCtx.NS.Name

	cfg := newKubeSchedulerConfig(withTopologyPlacementGenerator)

	readonlyClient, err := simulator.NewReadonlyClient(testCtx.KubeConfig)
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	node1Capacity10CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node2Capacity2CPU := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node2Capacity6CPU := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "6",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	pg1 := makePodGroup(ns, "workload-1", "", 2, "")
	pg2Single := makePodGroup(ns, "workload-2-single", "", 1, "")
	pg2Gang := makePodGroup(ns, "workload-2-gang", "", 2, "")

	v1Node1Pod8CPU := makeScheduledPod(ns, "v1-node1-8cpu", "node1", "", "8")
	v2Node1Pod2CPU := makeScheduledPod(ns, "v2-node1-2cpu", "node1", "", "2")
	v3Node2Pod4CPU := makeScheduledPod(ns, "v3-node2-4cpu", "node2", "", "4")
	stayNode1Pod2CPU := makeScheduledPod(ns, "stay-node1-2cpu", "node1", "", "2")
	stayNode2Pod2CPU := makeScheduledPod(ns, "stay-node2-2cpu", "node2", "", "2")

	tests := []struct {
		name                       string
		nodes                      []*v1.Node
		existingPods               []*v1.Pod
		podGroups                  []*schedulingv1beta1.PodGroup
		w1Pods                     []*v1.Pod
		w1PotentialVictims         []snapshot.PreemptionVictim
		w2Pods                     []*v1.Pod
		w2PotentialVictims         []snapshot.PreemptionVictim
		wantW1Victims              []string
		wantW2StatusCode           fwk.Code
		wantW2WaitingForPreemption bool
		wantW2Victims              []string
		wantW2SelectedNodes        []string
	}{
		{
			name:         "workload 1 requires preemption, workload 2 does not and avoids workload 1 victim node",
			nodes:        []*v1.Node{node1Capacity10CPU, node2Capacity2CPU},
			existingPods: []*v1.Pod{v1Node1Pod8CPU, stayNode1Pod2CPU},
			podGroups:    []*schedulingv1beta1.PodGroup{pg1, pg2Single},
			w1Pods: []*v1.Pod{
				makePod(ns, "w1-pod-1", "workload-1", "3"),
				makePod(ns, "w1-pod-2", "workload-1", "3"),
			},
			w1PotentialVictims: []snapshot.PreemptionVictim{
				&integrationVictim{name: "v1", pods: []*v1.Pod{v1Node1Pod8CPU}},
			},
			// Once v1 is removed and w1 (6 CPU) is scheduled on node1, node1 is listed first and still
			// has 2 CPU free, and node2 also has 2 CPU free without touching any CommittedVictims.
			w2Pods: []*v1.Pod{
				makePod(ns, "w2-pod-1", "workload-2-single", "2"),
			},
			w2PotentialVictims:         nil,
			wantW1Victims:              []string{"v1"},
			wantW2StatusCode:           fwk.Success,
			wantW2WaitingForPreemption: false,
			wantW2Victims:              nil,
			wantW2SelectedNodes:        []string{"node2"},
		},
		{
			name:         "workload 1 requires preemption, workload 2 needs to wait for workload 1 victims to schedule",
			nodes:        []*v1.Node{node1Capacity10CPU, node2Capacity2CPU},
			existingPods: []*v1.Pod{v1Node1Pod8CPU, stayNode1Pod2CPU, stayNode2Pod2CPU},
			podGroups:    []*schedulingv1beta1.PodGroup{pg1, pg2Gang},
			w1Pods: []*v1.Pod{
				makePod(ns, "w1-pod-1", "workload-1", "2"),
				makePod(ns, "w1-pod-2", "workload-1", "2"),
			},
			w1PotentialVictims: []snapshot.PreemptionVictim{
				&integrationVictim{name: "v1", pods: []*v1.Pod{v1Node1Pod8CPU}},
			},
			// node2 is full (2/2 CPU used by stayNode2Pod2CPU), so w2 (4 CPU) must use the remaining
			// space freed by v1 on node1 after w1 (4 CPU) is scheduled there.
			w2Pods: []*v1.Pod{
				makePod(ns, "w2-pod-1", "workload-2-gang", "2"),
				makePod(ns, "w2-pod-2", "workload-2-gang", "2"),
			},
			w2PotentialVictims:         nil,
			wantW1Victims:              []string{"v1"},
			wantW2StatusCode:           fwk.Unschedulable,
			wantW2WaitingForPreemption: true,
			wantW2Victims:              nil,
			wantW2SelectedNodes:        []string{"node1", "node1"},
		},
		{
			name:         "workload 1 requires preemption, workload 2 requires additional preemption and returns new victims",
			nodes:        []*v1.Node{node1Capacity10CPU, node2Capacity6CPU},
			existingPods: []*v1.Pod{v1Node1Pod8CPU, v2Node1Pod2CPU, v3Node2Pod4CPU, stayNode2Pod2CPU},
			podGroups:    []*schedulingv1beta1.PodGroup{pg1, pg2Gang},
			w1Pods: []*v1.Pod{
				makePod(ns, "w1-pod-1", "workload-1", "2"),
				makePod(ns, "w1-pod-2", "workload-1", "2"),
			},
			w1PotentialVictims: []snapshot.PreemptionVictim{
				&integrationVictim{name: "v1", pods: []*v1.Pod{v1Node1Pod8CPU}},
				&integrationVictim{name: "v2", pods: []*v1.Pod{v2Node1Pod2CPU}},
				&integrationVictim{name: "v3", pods: []*v1.Pod{v3Node2Pod4CPU}},
			},
			// After v1 (8 CPU on node1) is removed and w1 (4 CPU) is scheduled on node1,
			// node1 has 4 CPU free and node2 has 0 CPU free.
			// w2 needs two 4-CPU pods (one on node1 and one on node2), so it must also preempt v3 on node2.
			w2Pods: []*v1.Pod{
				makePod(ns, "w2-pod-1", "workload-2-gang", "4"),
				makePod(ns, "w2-pod-2", "workload-2-gang", "4"),
			},
			w2PotentialVictims: []snapshot.PreemptionVictim{
				&integrationVictim{name: "v2", pods: []*v1.Pod{v2Node1Pod2CPU}},
				&integrationVictim{name: "v3", pods: []*v1.Pod{v3Node2Pod4CPU}},
			},
			wantW1Victims:              []string{"v1"},
			wantW2StatusCode:           fwk.Unschedulable,
			wantW2WaitingForPreemption: true,
			wantW2Victims:              []string{"v3"},
			wantW2SelectedNodes:        []string{"node1", "node2"},
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
			for _, p := range tt.existingPods {
				if err := cs.Cache.AddPod(logger, p.DeepCopy()); err != nil {
					t.Fatalf("AddPod(%s) failed: %v", p.Name, err)
				}
			}
			for _, pg := range tt.podGroups {
				cs.Cache.AddPodGroup(pg)
			}

			snap := cs.GetAssociatedSnapshot()
			if err := cs.SyncSnapshot(logger); err != nil {
				t.Fatalf("SyncSnapshot failed: %v", err)
			}

			// Schedule Workload 1.
			w1Opts := snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values(tt.w1PotentialVictims),
				},
			}
			w1Res := snap.ScheduleWorkload(ctx, tt.w1Pods, w1Opts)
			if w1Res.Status.IsError() {
				t.Fatalf("Workload 1 ScheduleWorkload failed: %v", w1Res.Status.AsError())
			}
			if w1Res.Status.Code() != fwk.Unschedulable {
				t.Fatalf("Workload 1 Status.Code() = %v, want Unschedulable (status: %v)", w1Res.Status.Code(), w1Res.Status)
			}
			if !w1Res.WaitingForPreemption {
				t.Errorf("Workload 1 WaitingForPreemption = false, want true")
			}
			var gotW1Victims []string
			for _, v := range w1Res.PreemptionVictims {
				gotW1Victims = append(gotW1Victims, v.(*integrationVictim).name)
			}
			if !slices.Equal(gotW1Victims, tt.wantW1Victims) {
				t.Fatalf("Workload 1 PreemptionVictims = %v, want %v", gotW1Victims, tt.wantW1Victims)
			}
			if len(w1Res.PodResults) != len(tt.w1Pods) {
				t.Fatalf("Workload 1 len(PodResults) = %d, want %d", len(w1Res.PodResults), len(tt.w1Pods))
			}

			// Incrementally remove Workload 1's PreemptionVictims from the snapshot,
			// place Workload 1's pods onto their selected nodes in the snapshot,
			// and pass the victims as CommittedVictims for Workload 2.
			var committedVictimPods []*v1.Pod
			for _, v := range w1Res.PreemptionVictims {
				committedVictimPods = append(committedVictimPods, v.Pods()...)
			}
			if _, err := snap.PreemptPods(ctx, committedVictimPods); err != nil {
				t.Fatalf("PreemptPods(committedVictimPods) failed: %v", err)
			}
			for _, pRes := range w1Res.PodResults {
				if !pRes.Status.IsSuccess() {
					t.Fatalf("Workload 1 pod %s expected success, got %v", pRes.Pod.Name, pRes.Status)
				}
				placement, err := snap.MakePlacement([]string{pRes.SelectedNodeName})
				if err != nil {
					t.Fatalf("MakePlacement(%s) for Workload 1 pod %s failed: %v", pRes.SelectedNodeName, pRes.Pod.Name, err)
				}
				schedRes, err := snap.SchedulePods(ctx, []*v1.Pod{pRes.Pod}, placement, snapshot.SchedulePodsOptions{})
				if err != nil {
					t.Fatalf("SchedulePods(%s) on %s failed: %v", pRes.Pod.Name, pRes.SelectedNodeName, err)
				}
				if len(schedRes) != 1 || !schedRes[0].Status.IsSuccess() {
					t.Fatalf("SchedulePods(%s) on %s did not succeed: %v", pRes.Pod.Name, pRes.SelectedNodeName, schedRes)
				}
			}

			w2Opts := snapshot.ScheduleWorkloadOptions{
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					CommittedVictims: w1Res.PreemptionVictims,
				},
			}
			if tt.w2PotentialVictims != nil {
				w2Opts.PotentialVictims = slices.Values(tt.w2PotentialVictims)
			}

			// Schedule Workload 2.
			w2Res := snap.ScheduleWorkload(ctx, tt.w2Pods, w2Opts)
			if w2Res.Status.IsError() {
				t.Fatalf("Workload 2 ScheduleWorkload failed: %v", w2Res.Status.AsError())
			}
			if w2Res.Status.Code() != tt.wantW2StatusCode {
				t.Fatalf("Workload 2 Status.Code() = %v, want %v (status: %v)", w2Res.Status.Code(), tt.wantW2StatusCode, w2Res.Status)
			}
			if w2Res.WaitingForPreemption != tt.wantW2WaitingForPreemption {
				t.Errorf("Workload 2 WaitingForPreemption = %v, want %v", w2Res.WaitingForPreemption, tt.wantW2WaitingForPreemption)
			}

			var gotW2Victims []string
			for _, v := range w2Res.PreemptionVictims {
				gotW2Victims = append(gotW2Victims, v.(*integrationVictim).name)
			}
			if !slices.Equal(gotW2Victims, tt.wantW2Victims) {
				t.Fatalf("Workload 2 PreemptionVictims = %v, want %v", gotW2Victims, tt.wantW2Victims)
			}

			if len(w2Res.PodResults) != len(tt.w2Pods) {
				t.Fatalf("Workload 2 len(PodResults) = %d, want %d", len(w2Res.PodResults), len(tt.w2Pods))
			}
			var gotW2SelectedNodes []string
			for _, pRes := range w2Res.PodResults {
				if !pRes.Status.IsSuccess() {
					t.Errorf("Workload 2 pod %s expected success, got %v", pRes.Pod.Name, pRes.Status)
				}
				gotW2SelectedNodes = append(gotW2SelectedNodes, pRes.SelectedNodeName)
			}
			slices.Sort(gotW2SelectedNodes)
			if !slices.Equal(gotW2SelectedNodes, tt.wantW2SelectedNodes) {
				t.Errorf("Workload 2 selected nodes = %v, want %v", gotW2SelectedNodes, tt.wantW2SelectedNodes)
			}
		})
	}
}
