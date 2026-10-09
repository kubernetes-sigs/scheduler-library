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

			res, err := snap.ScheduleWorkload(ctx, tt.pods, snapshot.NewScheduleWorkloadOptions(false))
			if err != nil {
				t.Fatalf("ScheduleWorkload failed: %v", err)
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
	minRequiredCPU int64
	preemptedCPU   int64
}

func (f *integrationCPUPreemptionFilter) PreemptVictim(v snapshot.PreemptionVictim) {
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.preemptedCPU += cpu.Value()
		}
	}
}

func (f *integrationCPUPreemptionFilter) UnpreemptVictim(v snapshot.PreemptionVictim) {
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.preemptedCPU -= cpu.Value()
		}
	}
}

func (f *integrationCPUPreemptionFilter) MayFit() bool {
	return f.preemptedCPU >= f.minRequiredCPU
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
		name            string
		nodes           []*v1.Node
		existingPods    []*v1.Pod
		workloadPods    []*v1.Pod
		opts            snapshot.ScheduleWorkloadOptions
		wantSuccess     bool
		wantVictimNames []string
		probePodCPU     string
		wantProbeFits   bool
	}{
		{
			name:         "(a) fits without preemption",
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
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 2},
				},
			},
			wantSuccess:     true,
			wantVictimNames: nil,
			probePodCPU:     "1",
			wantProbeFits:   false,
		},
		{
			name:         "(b) fits after preempting subset of PotentialVictims with reprieve",
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
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 3},
				},
			},
			wantSuccess:     true,
			wantVictimNames: []string{"vMed"},
			probePodCPU:     "1",
			wantProbeFits:   false,
		},
		{
			name:         "(c) multi-pod PreemptionVictim preempted and restored atomically",
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
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 4},
				},
			},
			wantSuccess:     true,
			wantVictimNames: []string{"vSingle"},
			probePodCPU:     "1",
			wantProbeFits:   false,
		},
		{
			name:         "(d) CommittedVictims removed upfront and excluded from PreemptionVictims",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{vCommittedPod, v1Pod, v2Pod},
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
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 2},
				},
			},
			wantSuccess:     true,
			wantVictimNames: []string{"v1"},
			probePodCPU:     "1",
			wantProbeFits:   false,
		},
		{
			name:         "(e) fails when all PotentialVictims are insufficient and reverts snapshot",
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
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 2},
				},
			},
			wantSuccess:     false,
			wantVictimNames: nil,
			probePodCPU:     "2",
			wantProbeFits:   true,
		},
		{
			name:         "(f) DryRun with preemption returns victims and reverts snapshot",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				makePod(ns, "w-f1", "preempt-gang", "2"),
				makePod(ns, "w-f2", "preempt-gang", "2"),
			},
			opts: snapshot.ScheduleWorkloadOptions{
				DryRun: true,
				WorkloadPreemptionOptions: snapshot.WorkloadPreemptionOptions{
					PotentialVictims: slices.Values([]snapshot.PreemptionVictim{
						&integrationVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}},
						&integrationVictim{name: "vMed", pods: []*v1.Pod{vMedPod}},
						&integrationVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}},
					}),
					PreemptionFilter: &integrationCPUPreemptionFilter{minRequiredCPU: 3},
				},
			},
			wantSuccess:     true,
			wantVictimNames: []string{"vMed"},
			probePodCPU:     "2",
			wantProbeFits:   false,
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

			res, err := snap.ScheduleWorkload(ctx, tt.workloadPods, tt.opts)
			if err != nil {
				t.Fatalf("ScheduleWorkload failed: %v", err)
			}
			if res.Status.IsSuccess() != tt.wantSuccess {
				t.Fatalf("Status.IsSuccess() = %v, want %v (status: %v)", res.Status.IsSuccess(), tt.wantSuccess, res.Status)
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
