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
	"fmt"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
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

			snapshot, err := sim.NewClusterSnapshot(ctx, nil, nil)
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
		st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "110"}).Obj(),
	}

	snap, err := sim.NewClusterSnapshot(ctx, nil, nodes)
	if err != nil {
		t.Fatalf("failed to create snapshot: %v", err)
	}

	pod := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()

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

	node := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "110"}).Obj()
	state.Cache.AddNode(klog.FromContext(ctx), node)

	snap := state.GetAssociatedSnapshot()
	err = state.SyncSnapshot(klog.FromContext(ctx))
	if err != nil {
		t.Fatalf("failed to take snapshot: %v", err)
	}

	pod := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()

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
func newOutOfTreeSimulator(t *testing.T, client ReadonlyClient, registry frameworkruntime.Registry, extra ...Option) *SchedulingSimulator {
	t.Helper()

	plugins := &schedulerapi.Plugins{
		QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
		Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
	}
	var opts []Option
	if len(registry) > 0 {
		plugins.Filter = schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: outOfTreeFilterName}}}
		opts = append(opts, WithOutOfTreeRegistry(registry))
	}
	opts = append(opts, extra...)
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

			snap, err := sim.NewClusterSnapshot(ctx, nil, nodes)
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
		if _, err := sim.NewClusterSnapshot(ctx, nil, testNodes("node1")); err != nil {
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
	if _, err := sim.NewClusterSnapshot(ctx, nil, testNodes("node1")); err != nil {
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
