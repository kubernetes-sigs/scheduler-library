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

package simulator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

func minimalConfig() *schedulerapi.KubeSchedulerConfiguration {
	return &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: "default-scheduler",
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
				},
			},
		},
	}
}

func TestNewSchedulingSimulator(t *testing.T) {
	cfg := minimalConfig()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := NewSchedulingSimulator(t.Context(), cfg, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}
	if sim == nil {
		t.Fatal("Expected simulator to be non-nil")
	}
}

func TestNewSchedulingSimulatorWithNilInformerFactory(t *testing.T) {
	cfg := minimalConfig()
	sim, err := NewSchedulingSimulator(t.Context(), cfg, ReadonlyClient{client: fake.NewClientset()}, nil)
	if err != nil {
		t.Fatalf("failed to create simulator with nil informerFactory: %v", err)
	}
	if sim == nil {
		t.Fatal("Expected simulator to be non-nil")
	}
	if sim.informerFactory == nil {
		t.Error("Expected informerFactory to be automatically initialized, got nil")
	}
	if sim.comps == nil {
		t.Error("Expected comps to be automatically initialized, got nil")
	}

	_, err = sim.NewClusterState(t.Context())
	if err != nil {
		t.Fatalf("failed to create ClusterState: %v", err)
	}
}

func TestNewClusterState(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "error with invalid profile (non-existent plugin)",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "invalid-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "NonExistentPlugin"},
								},
							},
						},
					},
				},
			},
			expectErr: true,
		},
		{
			name: "success with multiple profiles",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "profile-1",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
					{
						SchedulerName: "profile-2",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			ctx := t.Context()

			sim, err := NewSchedulingSimulator(ctx, tc.cfg, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
			if tc.expectErr {
				if err != nil {
					return
				}
			} else if err != nil {
				t.Fatalf("NewSchedulingSimulator failed: %v", err)
			}

			state, err := sim.NewClusterState(ctx)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterState err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && state == nil {
				t.Fatal("Expected state to be non-nil")
			}
		})
	}

}

func TestNewClusterSnapshot(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "default-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "error with invalid profile (non-existent plugin)",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "invalid-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "NonExistentPlugin"},
								},
							},
						},
					},
				},
			},
			expectErr: true,
		},
		{
			name: "success with multiple profiles",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "profile-1",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
					{
						SchedulerName: "profile-2",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "PrioritySort"},
								},
							},
							Bind: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "DefaultBinder"},
								},
							},
						},
					},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			ctx := t.Context()

			sim, err := NewSchedulingSimulator(ctx, tc.cfg, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
			if tc.expectErr {
				if err != nil {
					return
				}
			} else if err != nil {
				t.Fatalf("NewSchedulingSimulator failed: %v", err)
			}

			snapshot, err := sim.NewClusterSnapshot(ctx, nil, nil, nil, nil)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterSnapshot err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && snapshot == nil {
				t.Fatal("Expected snapshot to be non-nil")
			}
		})
	}

}

func TestNewClusterSnapshot_Scheduling(t *testing.T) {
	ctx := context.Background()
	cfg := minimalConfig()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := NewSchedulingSimulator(ctx, cfg, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}

	nodes := []*v1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			Status: v1.NodeStatus{
				Allocatable: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
				Capacity: v1.ResourceList{
					v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
				},
			},
		},
	}

	snap, err := sim.NewClusterSnapshot(ctx, nil, nodes, nil, nil)
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			UID:       types.UID("uid-pod1"),
		},
	}

	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}
	results, err := snap.SchedulePods(ctx, []*v1.Pod{pod}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if !results[0].Status.IsSuccess() {
		t.Errorf("Expected scheduling success, got: %v", results[0].Status)
	}
	if results[0].SelectedNodeName != "node1" {
		t.Errorf("Expected pod to be scheduled on node1, got %q", results[0].SelectedNodeName)
	}
}

func TestClusterState_Scheduling(t *testing.T) {
	ctx := context.Background()
	cfg := minimalConfig()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := NewSchedulingSimulator(ctx, cfg, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}

	state, err := sim.NewClusterState(ctx)
	if err != nil {
		t.Fatalf("failed to create cluster state: %v", err)
	}

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		Status: v1.NodeStatus{
			Allocatable: v1.ResourceList{
				v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
			},
			Capacity: v1.ResourceList{
				v1.ResourcePods: *resource.NewQuantity(110, resource.DecimalSI),
			},
		},
	}
	state.Cache.AddNode(klog.FromContext(ctx), node)

	snap := state.GetAssociatedSnapshot()
	err = state.SyncSnapshot(klog.FromContext(ctx))
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			UID:       types.UID("uid-pod1"),
		},
	}

	placement, err := snap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}
	results, err := snap.SchedulePods(ctx, []*v1.Pod{pod}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Expected 1 result, got %d", len(results))
	}
	if !results[0].Status.IsSuccess() {
		t.Errorf("Expected scheduling success, got: %v", results[0].Status)
	}
	if results[0].SelectedNodeName != "node1" {
		t.Errorf("Expected pod to be scheduled on node1, got %q", results[0].SelectedNodeName)
	}
}

func TestNewClusterSnapshot_PodGroupScheduling(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload:                 true,
		features.TopologyAwareWorkloadScheduling: true,
		features.CompositePodGroup:               true,
	})

	ctx := context.Background()
	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: "default-scheduler",
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
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := NewSchedulingSimulator(ctx, cfg, ReadonlyClient{client: client}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}

	nodes := []*v1.Node{st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()}

	pg := testutils.MakeGangPodGroup("test-gang", "", 2)

	snap, err := sim.NewClusterSnapshot(ctx, nil, nodes, []*schedulingv1beta1.PodGroup{pg}, nil)
	if err != nil {
		t.Fatalf("failed to create snapshot with pod groups: %v", err)
	}

	pod1 := testutils.MakePod("pod1", "test-gang", "1")
	pod2 := testutils.MakePod("pod2", "test-gang", "1")

	results, err := snap.ScheduleWorkload(ctx, []*v1.Pod{pod1, pod2}, snapshot.NewScheduleWorkloadOptions(false))
	if err != nil {
		t.Fatalf("ScheduleWorkload failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Status.IsSuccess() {
			t.Errorf("Expected pod %s to schedule successfully, got: %v", r.Pod.Name, r.Status)
		}
		if r.SelectedNodeName != "node1" {
			t.Errorf("Expected pod %s on node1, got %q", r.Pod.Name, r.SelectedNodeName)
		}
	}
}

func TestInformersRunForTheSimulatorsLifetime(t *testing.T) {
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	simulatorCtx, shutdown := context.WithCancel(t.Context())
	sim, err := NewSchedulingSimulator(simulatorCtx, minimalConfig(), ReadonlyClient{client: client}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}

	snapshotCtx, cancel := context.WithCancel(t.Context())
	if _, err := sim.NewClusterSnapshot(snapshotCtx, nil, nil, nil, nil); err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}
	cancel()

	// Building the profiles registers a CSINode informer on the shared factory, for the lister
	// NodeVolumeLimits runs on, so the snapshot call is what started it.
	csiNodes := informerFactory.Storage().V1().CSINodes().Informer()
	if !csiNodes.HasSynced() {
		t.Fatal("Expected building the profiles to have registered and started a CSINode informer")
	}

	// The call it was registered by is over, but the informer belongs to the simulator, so it
	// keeps watching.
	csiNode := &storagev1.CSINode{ObjectMeta: metav1.ObjectMeta{Name: "node1"}}
	if _, err := client.StorageV1().CSINodes().Create(t.Context(), csiNode, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create CSINode: %v", err)
	}
	if err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, wait.ForeverTestTimeout, true,
		func(context.Context) (bool, error) {
			_, seen, err := csiNodes.GetStore().GetByKey(csiNode.Name)
			return seen, err
		}); err != nil {
		t.Errorf("The informer never saw a CSINode created after the snapshot that registered it was cancelled (%v), so it stops with that call", err)
	}

	shutdown()
	if err := wait.PollUntilContextTimeout(t.Context(), 10*time.Millisecond, wait.ForeverTestTimeout, true,
		func(context.Context) (bool, error) {
			return csiNodes.IsStopped(), nil
		}); err != nil {
		t.Errorf("The informer is still running after the simulator was shut down (%v), so it never stops", err)
	}
}

func TestNewClusterSnapshotFailsOnceTheSimulatorContextIsDone(t *testing.T) {
	client := fake.NewClientset()
	simulatorCtx, shutdown := context.WithCancel(t.Context())
	sim, err := NewSchedulingSimulator(simulatorCtx, minimalConfig(), ReadonlyClient{client: client}, informers.NewSharedInformerFactory(client, 0))
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}
	shutdown()

	returned := make(chan error, 1)
	go func() {
		_, err := sim.NewClusterSnapshot(t.Context(), nil, nil, nil, nil)
		returned <- err
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the simulator's context is done") {
			t.Errorf("Expected the simulator's shutdown to be reported as the cause, got %v", err)
		}
	case <-time.After(wait.ForeverTestTimeout):
		t.Fatal("NewClusterSnapshot did not return; it is waiting for informers that can no longer sync")
	}
}

func TestMultipleSnapshotsAndStates_NoInformerIndexerPanic(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	sim, err := NewSchedulingSimulator(ctx, nil, ReadonlyClient{client: fake.NewClientset()}, informerFactory)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}

	// Calling NewClusterSnapshot multiple times on the same SchedulingSimulator
	// must not panic due to informer indexer conflict or already started informers.
	for i := range 3 {
		snap, err := sim.NewClusterSnapshot(ctx, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("iteration %d: NewClusterSnapshot failed: %v", i, err)
		}
		if snap == nil {
			t.Fatalf("iteration %d: Expected snapshot to be non-nil", i)
		}
	}

	// Calling NewClusterState multiple times on the same SchedulingSimulator
	// must also not panic and create isolated states.
	for i := range 3 {
		st, err := sim.NewClusterState(ctx)
		if err != nil {
			t.Fatalf("iteration %d: NewClusterState failed: %v", i, err)
		}
		if st == nil {
			t.Fatalf("iteration %d: Expected state to be non-nil", i)
		}
	}
}

func TestDRASnapshotIsolation(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.DynamicResourceAllocation: true,
	})

	ctx := t.Context()
	driverName := "test-driver.cdi.k8s.io"
	className := "dra-test-class"
	nodeCapacity := map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}

	deviceClass := &resourceapi.DeviceClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: className,
		},
	}
	node := st.MakeNode().Name("node1").Label("kubernetes.io/hostname", "node1").Capacity(nodeCapacity).Obj()

	// Single device "instance-1" on node1 so only one DRA pod can fit per snapshot.
	slice := st.MakeResourceSlice("node1", driverName).Device("instance-1").Obj()

	claim1 := st.MakeResourceClaim().
		Name("claim-1").
		Namespace("default").
		UID("uid-claim-1").
		Request(className).
		Obj()
	claim2 := st.MakeResourceClaim().
		Name("claim-2").
		Namespace("default").
		UID("uid-claim-2").
		Request(className).
		Obj()

	makePodWithClaim := func(podName, podUID, claimName string) *v1.Pod {
		resourceClaimName := "my-dra-res"

		pod := st.MakePod().Name(podName).Namespace("default").
			UID(podUID).
			PodResourceClaims(v1.PodResourceClaim{Name: resourceClaimName, ResourceClaimName: new(claimName)}).
			Obj()
		pod.Spec.Containers = []v1.Container{
			{
				Name: "c1",
				Resources: v1.ResourceRequirements{
					Claims: []v1.ResourceClaim{{Name: resourceClaimName}},
				},
			},
		}
		return pod
	}

	pod1 := makePodWithClaim("pod-1", "uid-pod-1", "claim-1")
	pod2 := makePodWithClaim("pod-2", "uid-pod-2", "claim-2")

	client := fake.NewClientset(deviceClass, slice, claim1, claim2)
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	// nil cfg applies default kube-scheduler profile (which includes DynamicResources plugin).
	sim, err := NewSchedulingSimulator(ctx, nil, ReadonlyClient{client: client}, informerFactory)
	if err != nil {
		t.Fatalf("NewSchedulingSimulator failed: %v", err)
	}

	// 1. In snap1, SchedulePods(pod1) reserves the only device ("instance-1") on node1
	// (via DynamicResources.Reserve -> SignalClaimPendingAllocation into snap1's inFlightAllocations).
	snap1, err := sim.NewClusterSnapshot(ctx, nil, []*v1.Node{node}, nil, nil)
	if err != nil {
		t.Fatalf("snap1 NewClusterSnapshot failed: %v", err)
	}
	placement1, err := snap1.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("snap1 MakePlacement failed: %v", err)
	}

	res1, err := snap1.SchedulePods(ctx, []*v1.Pod{pod1}, placement1, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("snap1 SchedulePods(pod1) failed: %v", err)
	}
	if len(res1) != 1 || !res1[0].Status.IsSuccess() {
		t.Fatalf("snap1 expected pod1 to schedule successfully, got: %+v", res1)
	}

	// Within the same snapshot (snap1), pod2 must fail because "instance-1" is already reserved by claim1.
	res1Pod2, err := snap1.SchedulePods(ctx, []*v1.Pod{pod2}, placement1, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("snap1 SchedulePods(pod2) failed: %v", err)
	}
	if len(res1Pod2) != 1 || res1Pod2[0].Status.IsSuccess() {
		t.Fatalf("snap1 expected pod2 to fail scheduling due to exhausted DRA device, but succeeded: %+v", res1Pod2)
	}

	// 2. In snap2 (created from the same simulator), DRA inFlightAllocations must be isolated
	// from snap1: pod2 (using claim2) must schedule successfully on node1.
	snap2, err := sim.NewClusterSnapshot(ctx, nil, []*v1.Node{node}, nil, nil)
	if err != nil {
		t.Fatalf("snap2 NewClusterSnapshot failed: %v", err)
	}
	placement2, err := snap2.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("snap2 MakePlacement failed: %v", err)
	}

	res2, err := snap2.SchedulePods(ctx, []*v1.Pod{pod2}, placement2, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("snap2 SchedulePods(pod2) failed: %v", err)
	}
	if len(res2) != 1 || !res2[0].Status.IsSuccess() {
		t.Fatalf("snap2 expected pod2 to schedule successfully (isolated from snap1 DRA allocation), got: %+v", res2)
	}
}

// stubDRAManager stands in for the informer-backed manager. The test only checks which
// manager the profiles were built with, so none of the accessors are ever called.
type stubDRAManager struct{}

var _ fwk.SharedDRAManager = &stubDRAManager{}

func (s *stubDRAManager) ResourceClaims() fwk.ResourceClaimTracker     { return nil }
func (s *stubDRAManager) ResourceSlices() fwk.ResourceSliceLister      { return nil }
func (s *stubDRAManager) DeviceClasses() fwk.DeviceClassLister         { return nil }
func (s *stubDRAManager) DeviceClassResolver() fwk.DeviceClassResolver { return nil }

func TestWithSharedDRAManager(t *testing.T) {
	stub := &stubDRAManager{}

	tests := []struct {
		name     string
		opts     []Option
		wantStub bool
	}{
		{
			name:     "profiles built with a custom DRA manager",
			opts:     []Option{WithSharedDRAManager(stub)},
			wantStub: true,
		},
		{
			name: "profiles built with the default informer-backed DRA manager",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			client := fake.NewClientset()
			sim, err := NewSchedulingSimulator(ctx, minimalConfig(), ReadonlyClient{client: client}, informers.NewSharedInformerFactory(client, 0))
			if err != nil {
				t.Fatalf("NewSchedulingSimulator failed: %v", err)
			}

			profiles, err := sim.buildProfileMap(ctx, cache.NewEmptySnapshot(), tc.opts...)
			if err != nil {
				t.Fatalf("buildProfileMap failed: %v", err)
			}

			got := profiles.Map["default-scheduler"].SharedDRAManager()
			switch {
			case tc.wantStub && got != stub:
				t.Errorf("SharedDRAManager() = %v, want the supplied stub", got)
			case !tc.wantStub && got == nil:
				t.Error("SharedDRAManager() = nil, want the informer-backed manager")
			case !tc.wantStub && got == stub:
				t.Error("SharedDRAManager() returned the stub, want the informer-backed manager")
			}
		})
	}
}

const outOfTreeFilterName = "RejectNodeFilter"

// nodeRejectingPlugin is an out-of-tree Filter plugin that rejects one specific node.
type nodeRejectingPlugin struct {
	rejectedNode string
}

var _ fwk.FilterPlugin = &nodeRejectingPlugin{}

func (p *nodeRejectingPlugin) Name() string { return outOfTreeFilterName }

func (p *nodeRejectingPlugin) Filter(_ context.Context, _ fwk.CycleState, _ *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	if nodeInfo.Node().Name == p.rejectedNode {
		return fwk.NewStatus(fwk.Unschedulable, "rejected by the out-of-tree plugin")
	}
	return nil
}

func registryWithNodeRejectingPlugin(rejectedNode string) frameworkruntime.Registry {
	return frameworkruntime.Registry{
		outOfTreeFilterName: func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
			return &nodeRejectingPlugin{rejectedNode: rejectedNode}, nil
		},
	}
}

// newOutOfTreeSimulator builds a simulator with a minimal profile. The out-of-tree Filter
// plugin is enabled only when a registry is given, so an empty registry yields the baseline
// behaviour of the same profile without the plugin.
func newOutOfTreeSimulator(t *testing.T, client ReadonlyClient, registry frameworkruntime.Registry) *SchedulingSimulator {
	t.Helper()

	plugins := &schedulerapi.Plugins{
		QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
		Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
	}
	var opts []SimulatorOption
	if len(registry) > 0 {
		plugins.Filter = schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: outOfTreeFilterName}}}
		opts = append(opts, WithOutOfTreeRegistry(registry))
	}
	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{SchedulerName: "default-scheduler", Plugins: plugins},
		},
	}

	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	sim, err := NewSchedulingSimulator(t.Context(), cfg, client, informerFactory, opts...)
	if err != nil {
		t.Fatalf("failed to create simulator: %v", err)
	}
	return sim
}

func testNodes(names ...string) []*v1.Node {
	nodes := make([]*v1.Node, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{v1.ResourcePods: "110"}).Obj())
	}
	return nodes
}

func TestOutOfTreePluginAffectsScheduling(t *testing.T) {
	nodes := testNodes("node1", "node2")

	tests := []struct {
		name string
		// rejectedNode is empty when no out-of-tree plugin is registered at all.
		rejectedNode string
		wantFeasible []string
	}{
		{
			name:         "no out-of-tree plugin leaves both nodes feasible",
			wantFeasible: []string{"node1", "node2"},
		},
		{
			name:         "out-of-tree plugin rejects node1",
			rejectedNode: "node1",
			wantFeasible: []string{"node2"},
		},
		{
			name:         "out-of-tree plugin rejects node2",
			rejectedNode: "node2",
			wantFeasible: []string{"node1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var registry frameworkruntime.Registry
			if tc.rejectedNode != "" {
				registry = registryWithNodeRejectingPlugin(tc.rejectedNode)
			}
			sim := newOutOfTreeSimulator(t, ReadonlyClient{client: fake.NewClientset()}, registry)

			snap, err := sim.NewClusterSnapshot(ctx, nil, nodes, nil, nil)
			if err != nil {
				t.Fatalf("failed to create snapshot: %v", err)
			}

			pod := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()
			placement, err := snap.MakePlacement([]string{"node1", "node2"})
			if err != nil {
				t.Fatalf("MakePlacement failed: %v", err)
			}

			feasible, _, err := snap.CanSchedulePod(ctx, pod, placement)
			if err != nil {
				t.Fatalf("CanSchedulePod failed: %v", err)
			}
			got := slices.Sorted(slices.Values(feasible))
			if !slices.Equal(got, tc.wantFeasible) {
				t.Errorf("CanSchedulePod feasible nodes = %v, want %v", got, tc.wantFeasible)
			}

			results, err := snap.SchedulePods(ctx, []*v1.Pod{pod}, placement, snapshot.SchedulePodsOptions{})
			if err != nil {
				t.Fatalf("SchedulePods failed: %v", err)
			}
			if len(results) != 1 {
				t.Fatalf("Expected 1 result, got %d", len(results))
			}
			if !results[0].Status.IsSuccess() {
				t.Fatalf("Expected scheduling success, got: %v", results[0].Status)
			}
			// Ties between equally scored nodes are broken at random, so only membership in
			// the feasible set is deterministic.
			if !slices.Contains(tc.wantFeasible, results[0].SelectedNodeName) {
				t.Errorf("Pod scheduled on %q, want one of %v", results[0].SelectedNodeName, tc.wantFeasible)
			}
		})
	}
}

func TestOutOfTreeRegistryCloned(t *testing.T) {
	ctx := t.Context()
	nodes := testNodes("node1", "node2")
	reg := registryWithNodeRejectingPlugin("node1")
	sim := newOutOfTreeSimulator(t, ReadonlyClient{client: fake.NewClientset()}, reg)

	// Mutate the original map after passing it into the option
	delete(reg, outOfTreeFilterName)

	snap, err := sim.NewClusterSnapshot(ctx, nil, nodes, nil, nil)
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	pod := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()
	placement, err := snap.MakePlacement([]string{"node1", "node2"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	feasible, _, err := snap.CanSchedulePod(ctx, pod, placement)
	if err != nil {
		t.Fatalf("CanSchedulePod failed: %v", err)
	}
	// "node1" should still be rejected because the simulator cloned the registry
	if slices.Contains(feasible, "node1") {
		t.Errorf("Expected node1 to be rejected despite caller deleting plugin from original map, got feasible: %v", feasible)
	}
}

func TestOutOfTreePluginFactoryCalledPerProfileMap(t *testing.T) {
	ctx := t.Context()
	var calls int
	registry := frameworkruntime.Registry{
		outOfTreeFilterName: func(_ context.Context, _ runtime.Object, _ fwk.Handle) (fwk.Plugin, error) {
			calls++
			return &nodeRejectingPlugin{rejectedNode: "node2"}, nil
		},
	}
	sim := newOutOfTreeSimulator(t, ReadonlyClient{client: fake.NewClientset()}, registry)
	if calls != 0 {
		t.Fatalf("Expected no plugin instantiation before a state or snapshot is built, got %d", calls)
	}

	// Every new profile map instantiates the plugin anew: the snapshot shared lister can only
	// be set at framework construction time.
	for i := 1; i <= 3; i++ {
		if _, err := sim.NewClusterSnapshot(ctx, nil, testNodes("node1"), nil, nil); err != nil {
			t.Fatalf("failed to create snapshot: %v", err)
		}
		if calls != i {
			t.Errorf("After %d NewClusterSnapshot calls, expected %d factory calls, got %d", i, i, calls)
		}
	}

	state, err := sim.NewClusterState(ctx)
	if err != nil {
		t.Fatalf("failed to create cluster state: %v", err)
	}
	if calls != 4 {
		t.Errorf("Expected NewClusterState to build a profile map, got %d factory calls", calls)
	}

	// ClusterState.SyncSnapshot reuses the profiles it was built with.
	if err := state.SyncSnapshot(klog.FromContext(ctx)); err != nil {
		t.Fatalf("failed to sync snapshot: %v", err)
	}
	if calls != 4 {
		t.Errorf("Expected ClusterState.SyncSnapshot to reuse the profile map, got %d factory calls", calls)
	}
}

// kubeConfigPlugin builds a clientset out of the config exposed via fwk.Handle, the way
// out-of-tree plugins with their own CRDs do.
type kubeConfigPlugin struct {
	client kubernetes.Interface
}

var _ fwk.FilterPlugin = &kubeConfigPlugin{}

func (p *kubeConfigPlugin) Name() string { return outOfTreeFilterName }

func (p *kubeConfigPlugin) Filter(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ fwk.NodeInfo) *fwk.Status {
	return nil
}

func TestOutOfTreePluginKubeConfigIsReadonly(t *testing.T) {
	ctx := t.Context()

	// The host is never dialed: the read-only round tripper rejects mutations before the
	// request leaves the process.
	readonlyClient, err := NewReadonlyClient(&rest.Config{Host: "https://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("NewReadonlyClient failed: %v", err)
	}

	var built *kubeConfigPlugin
	registry := frameworkruntime.Registry{
		outOfTreeFilterName: func(_ context.Context, _ runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
			cfg := h.KubeConfig()
			if cfg == nil {
				return nil, fmt.Errorf("handle.KubeConfig() is nil")
			}
			c, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				return nil, err
			}
			built = &kubeConfigPlugin{client: c}
			return built, nil
		},
	}

	sim := newOutOfTreeSimulator(t, readonlyClient, registry)
	if _, err := sim.NewClusterSnapshot(ctx, nil, testNodes("node1"), nil, nil); err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}
	if built == nil {
		t.Fatal("Expected the out-of-tree plugin to be instantiated")
	}

	pod := st.MakePod().Name("pod1").Namespace("default").Obj()
	_, err = built.client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	if err == nil {
		t.Fatal("Expected the plugin's client to reject a write, got nil error")
	}
	if !strings.Contains(err.Error(), "mutations are not supported in scheduler library") {
		t.Errorf("Expected a read-only transport error, got: %v", err)
	}
}
