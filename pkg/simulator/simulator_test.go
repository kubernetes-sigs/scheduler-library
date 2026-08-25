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
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func TestNewSchedulingSimulator(t *testing.T) {
	cfg := &schedulerapi.KubeSchedulerConfiguration{
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
	cfg := &schedulerapi.KubeSchedulerConfiguration{
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
	cfg := &schedulerapi.KubeSchedulerConfiguration{
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
	cfg := &schedulerapi.KubeSchedulerConfiguration{
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
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.GenericWorkload, true)
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.TopologyAwareWorkloadScheduling, true)

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

	nodes := []*v1.Node{
		st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
			v1.ResourceCPU:    "4",
			v1.ResourceMemory: "4Gi",
			v1.ResourcePods:   "10",
		}).Obj(),
	}

	pg := st.MakePodGroup().Name("test-gang").Namespace("default").MinCount(2).Obj()

	snap, err := sim.NewClusterSnapshot(ctx, nil, nodes, []*schedulingv1beta1.PodGroup{pg}, nil)
	if err != nil {
		t.Fatalf("failed to create snapshot with pod groups: %v", err)
	}

	pod1 := st.MakePod().Name("pod1").Namespace("default").PodGroupName("test-gang").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").PodGroupName("test-gang").Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()

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
