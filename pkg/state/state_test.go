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

package state

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	plugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	slframework "sigs.k8s.io/scheduler-library/pkg/framework"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func init() {
	slframework.InitMetricsOnce()
}

func TestClusterState_AddPod(t *testing.T) {
	tests := []struct {
		name         string
		existingPods []*v1.Pod
		podToAdd     *v1.Pod
		wantErr      bool
		expected     map[string]sets.Set[string]
	}{
		{
			name:     "add unassigned pod",
			podToAdd: st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj(),
			expected: map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name:     "add assigned pod",
			podToAdd: st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			expected: map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name: "add duplicate pod",
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			},
			podToAdd: st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			wantErr:  true,
			expected: map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()
			state := New(cache.New(ctx, nil, false, false), newDummyProfileMap(), sharedSnap)

			state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())

			for _, p := range tc.existingPods {
				if err := state.Cache.AddPod(logger, p); err != nil {
					t.Fatalf("Failed to add existing pod: %v", err)
				}
			}

			err := state.Cache.AddPod(logger, tc.podToAdd)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AddPod() error = %v, wantErr = %v", err, tc.wantErr)
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			err = state.SyncSnapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}

			ft.VerifySnapshot(t, sharedSnap, tc.expected)
		})
	}
}

func TestClusterState_RemovePod(t *testing.T) {
	tests := []struct {
		name         string
		existingPods []*v1.Pod
		podToRemove  *v1.Pod
		wantErr      bool
		expected     map[string]sets.Set[string]
	}{
		{
			name: "remove existing pod",
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			},
			podToRemove: st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			expected:    map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name: "remove non-existent pod",
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			},
			podToRemove: st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Node("node1").Obj(),
			wantErr:     true,
			expected:    map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()
			state := New(cache.New(ctx, nil, false, false), newDummyProfileMap(), sharedSnap)

			state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())

			for _, p := range tc.existingPods {
				if err := state.Cache.AddPod(logger, p); err != nil {
					t.Fatalf("Failed to add pod: %v", err)
				}
			}

			err := state.Cache.RemovePod(logger, tc.podToRemove)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemovePod() error = %v, wantErr = %v", err, tc.wantErr)
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			err = state.SyncSnapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}

			ft.VerifySnapshot(t, sharedSnap, tc.expected)
		})
	}
}

func TestClusterState_AddNode(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		nodeToAdd     *v1.Node
		expected      map[string]sets.Set[string]
	}{
		{
			name: "add valid node",
			nodeToAdd: st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
				v1.ResourceCPU:    "1",
				v1.ResourceMemory: "1Gi",
				v1.ResourcePods:   "110",
			}).Obj(),
			expected: map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name: "add duplicate node",
			existingNodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    "1",
					v1.ResourceMemory: "1Gi",
					v1.ResourcePods:   "110",
				}).Obj(),
			},
			nodeToAdd: st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
				v1.ResourceCPU:    "1",
				v1.ResourceMemory: "1Gi",
				v1.ResourcePods:   "110",
			}).Obj(),
			expected: map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()
			state := New(cache.New(ctx, nil, false, false), newDummyProfileMap(), sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}

			state.Cache.AddNode(logger, tc.nodeToAdd)

			ft.VerifySnapshot(t, sharedSnap, nil)
			err := state.SyncSnapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}

			ft.VerifySnapshot(t, sharedSnap, tc.expected)
		})
	}
}

func TestClusterState_RemoveNode(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		nodeToRemove  *v1.Node
		wantErr       bool
		expected      map[string]sets.Set[string]
	}{
		{
			name: "remove existing node",
			existingNodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
			},
			nodeToRemove: st.MakeNode().Name("node1").Obj(),
			expected:     map[string]sets.Set[string]{},
		},
		{
			name: "remove non-existent node",
			existingNodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
			},
			nodeToRemove: st.MakeNode().Name("node2").Obj(),
			wantErr:      true,
			expected:     map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()
			state := New(cache.New(ctx, nil, false, false), newDummyProfileMap(), sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}

			err := state.Cache.RemoveNode(logger, tc.nodeToRemove)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveNode() error = %v, wantErr = %v", err, tc.wantErr)
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			err = state.SyncSnapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}

			ft.VerifySnapshot(t, sharedSnap, tc.expected)
		})
	}
}

func TestClusterState_Snapshot(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		existingPods  []*v1.Pod
		hasFramework  bool
		expected      map[string]sets.Set[string]
	}{
		{
			name:     "empty snapshot",
			expected: map[string]sets.Set[string]{},
		},
		{
			name: "snapshot with data",
			existingNodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			},
			expected: map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name: "snapshot in sync with framework snapshot",
			existingNodes: []*v1.Node{
				st.MakeNode().Name("node1").Obj(),
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			},
			hasFramework: true,
			expected:     map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)

			var profiles *upstreamsync.ProfileMap
			sharedSnap := cache.NewEmptySnapshot()

			if tc.hasFramework {
				informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
				registry := plugins.NewInTreeRegistry()
				prof := schedulerapi.KubeSchedulerProfile{
					SchedulerName: "default-scheduler",
				}
				fwk, err := frameworkruntime.NewFramework(ctx, registry, &prof,
					frameworkruntime.WithSnapshotSharedLister(sharedSnap),
					frameworkruntime.WithInformerFactory(informerFactory),
				)
				if err != nil {
					t.Fatalf("Failed to create framework: %v", err)
				}
				profiles = &upstreamsync.ProfileMap{
					Map: profile.Map{
						"default-scheduler": fwk,
					},
				}
			} else {
				profiles = newDummyProfileMap()
			}

			state := New(cache.New(ctx, nil, false, false), profiles, sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}
			for _, p := range tc.existingPods {
				if err := state.Cache.AddPod(logger, p); err != nil {
					t.Fatalf("Failed to add pod: %v", err)
				}
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			err := state.SyncSnapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}

			ft.VerifySnapshot(t, sharedSnap, tc.expected)
		})
	}
}

func TestClusterState_SequentialUpdates(t *testing.T) {
	type action func(t *testing.T, state *ClusterState)

	addPod := func(pod *v1.Pod) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			if err := state.Cache.AddPod(klog.FromContext(t.Context()), pod); err != nil {
				t.Fatalf("AddPod() error = %v", err)
			}
		}
	}
	addNode := func(node *v1.Node) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			state.Cache.AddNode(klog.FromContext(t.Context()), node)
		}
	}
	removePod := func(pod *v1.Pod) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			if err := state.Cache.RemovePod(klog.FromContext(t.Context()), pod); err != nil {
				t.Fatalf("RemovePod() error = %v", err)
			}
		}
	}
	removeNode := func(node *v1.Node) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			if err := state.Cache.RemoveNode(klog.FromContext(t.Context()), node); err != nil {
				t.Fatalf("RemoveNode() error = %v", err)
			}
		}
	}

	updateSnapshot := func() action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			err := state.SyncSnapshot(klog.FromContext(t.Context()))
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
		}
	}

	assertSnapshot := func(expected map[string]sets.Set[string]) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			ft.VerifySnapshot(t, state.snapshotData, expected)
		}
	}

	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "1",
		v1.ResourceMemory: "1Gi",
		v1.ResourcePods:   "110",
	}).Obj()

	tests := []struct {
		name  string
		steps []action
	}{
		{
			name: "pod assigned to non-existent node then node added",
			steps: []action{
				addPod(pod1),
				updateSnapshot(),
				assertSnapshot(map[string]sets.Set[string]{}),
				addNode(node1),
				updateSnapshot(),
				assertSnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name: "node removal while pods still exist",
			steps: []action{
				addNode(node1),
				addPod(pod1),
				updateSnapshot(),
				assertSnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
				removeNode(node1),
				updateSnapshot(),
				assertSnapshot(map[string]sets.Set[string]{}),
				removePod(pod1),
				updateSnapshot(),
				assertSnapshot(map[string]sets.Set[string]{}),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			sharedSnap := cache.NewEmptySnapshot()
			state := New(cache.New(ctx, nil, false, false), newDummyProfileMap(), sharedSnap)

			for _, step := range tc.steps {
				step(t, state)
			}
		})
	}
}

func newDummyProfileMap() *upstreamsync.ProfileMap {
	return &upstreamsync.ProfileMap{
		Map: make(profile.Map),
	}
}

func TestClusterState_SyncSnapshot_RevertsMutations(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)
	sharedSnap := cache.NewEmptySnapshot()

	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	registry := plugins.NewInTreeRegistry()
	prof := schedulerapi.KubeSchedulerProfile{
		SchedulerName: "default-scheduler",
	}
	fwk, err := frameworkruntime.NewFramework(ctx, registry, &prof,
		frameworkruntime.WithSnapshotSharedLister(sharedSnap),
		frameworkruntime.WithInformerFactory(informerFactory),
	)
	if err != nil {
		t.Fatalf("Failed to create framework: %v", err)
	}
	profiles := &upstreamsync.ProfileMap{
		Map: profile.Map{
			"default-scheduler": fwk,
		},
	}

	state := New(cache.New(ctx, nil, false, false), profiles, sharedSnap)

	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Obj()

	state.Cache.AddNode(logger, node1)
	if err := state.Cache.AddPod(logger, pod1); err != nil {
		t.Fatalf("AddPod failed: %v", err)
	}

	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("pod1")})

	csnap := state.GetAssociatedSnapshot()

	placement, err := csnap.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	// Mutate snapshot by scheduling pod2
	_, err = csnap.SchedulePods(ctx, []*v1.Pod{pod2}, placement, snapshot.SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")})

	// Calling SyncSnapshot must revert snapshot mutations (pod2 scheduling)
	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("SyncSnapshot failed: %v", err)
	}

	csnap2 := state.GetAssociatedSnapshot()
	if csnap2 != csnap {
		t.Errorf("Expected SyncSnapshot to return the same snapshot instance")
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("pod1")})
}

const testReservePluginName = "TestReservePlugin"

type mockReservePlugin struct {
	reserveCalled   bool
	unreserveCalled bool
	failReserve     bool
}

func (p *mockReservePlugin) Name() string {
	return testReservePluginName
}

var _ fwk.ReservePlugin = &mockReservePlugin{}

func (p *mockReservePlugin) Reserve(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ string) *fwk.Status {
	p.reserveCalled = true
	if p.failReserve {
		return fwk.NewStatus(fwk.Error, "reserve plugin failed")
	}
	return fwk.NewStatus(fwk.Success)
}

func (p *mockReservePlugin) Unreserve(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ string) {
	p.unreserveCalled = true
}

func newTestProfile(schedulerName string, reservePluginName string) *schedulerapi.KubeSchedulerProfile {
	prof := &schedulerapi.KubeSchedulerProfile{
		SchedulerName: schedulerName,
	}
	if reservePluginName != "" {
		prof.Plugins = &schedulerapi.Plugins{
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
			Reserve: schedulerapi.PluginSet{
				Enabled: []schedulerapi.Plugin{
					{Name: reservePluginName},
				},
			},
		}
	}
	return prof
}

func newTestProfileMap(ctx context.Context, sharedSnap *cache.Snapshot, registry frameworkruntime.Registry, prof *schedulerapi.KubeSchedulerProfile) (*upstreamsync.ProfileMap, error) {
	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	if registry == nil {
		registry = plugins.NewInTreeRegistry()
	}
	if prof == nil {
		prof = newTestProfile(v1.DefaultSchedulerName, "")
	}
	fwk, err := frameworkruntime.NewFramework(ctx, registry, prof,
		frameworkruntime.WithSnapshotSharedLister(sharedSnap),
		frameworkruntime.WithInformerFactory(informerFactory),
	)
	if err != nil {
		return nil, err
	}
	return &upstreamsync.ProfileMap{
		Map: profile.Map{
			prof.SchedulerName: fwk,
		},
	}, nil
}

func TestClusterState_AssumeAndReserve(t *testing.T) {
	tests := []struct {
		name                 string
		nodeNames            []string
		pod                  *v1.Pod
		plugin               *mockReservePlugin
		withCustomProfileMap bool
		profileSchedulerName string
		nilCache             bool
		nilProfiles          bool
		nilCycleState        bool
		duplicateAssume      bool
		wantErr              bool
		wantReserveCalled    bool
		wantUnreserveCalled  bool
		wantAssumedInCache   bool
		expectedSnapshot     map[string]sets.Set[string]
	}{
		{
			name:                 "successful assume and reserve",
			nodeNames:            []string{"node1"},
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			plugin:               &mockReservePlugin{},
			withCustomProfileMap: true,
			wantErr:              false,
			wantReserveCalled:    true,
			wantUnreserveCalled:  false,
			wantAssumedInCache:   true,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name:      "reserve plugin fails and rolls back",
			nodeNames: []string{"node1"},
			pod:       st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			plugin: &mockReservePlugin{
				failReserve: true,
			},
			withCustomProfileMap: true,
			wantErr:              true,
			wantReserveCalled:    true,
			wantUnreserveCalled:  true,
			wantAssumedInCache:   false,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name:      "pod without node name fails",
			nodeNames: []string{"node1"},
			pod:       st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj(),
			wantErr:   true,
		},
		{
			name:      "nil pod fails",
			nodeNames: []string{"node1"},
			pod:       nil,
			wantErr:   true,
		},
		{
			name:                 "profile not found for pod scheduler name",
			nodeNames:            []string{"node1"},
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").SchedulerName("custom-scheduler").Obj(),
			withCustomProfileMap: true,
			profileSchedulerName: v1.DefaultSchedulerName,
			wantErr:              true,
		},
		{
			name:                 "duplicate assume returns error",
			nodeNames:            []string{"node1"},
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			plugin:               &mockReservePlugin{},
			withCustomProfileMap: true,
			duplicateAssume:      true,
			wantErr:              true,
			wantReserveCalled:    true,
			wantAssumedInCache:   true,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name:     "nil cache returns error",
			pod:      st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			nilCache: true,
			wantErr:  true,
		},
		{
			name:        "nil profiles returns error",
			pod:         st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			nilProfiles: true,
			wantErr:     true,
		},
		{
			name:          "nil cycleState returns error",
			pod:           st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			nilCycleState: true,
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()

			var profiles *upstreamsync.ProfileMap
			var err error
			if tc.nilProfiles {
				profiles = nil
			} else if tc.withCustomProfileMap {
				registry := plugins.NewInTreeRegistry()
				schedulerName := v1.DefaultSchedulerName
				if tc.profileSchedulerName != "" {
					schedulerName = tc.profileSchedulerName
				}
				var reservePluginName string
				if tc.plugin != nil {
					if err := registry.Register(testReservePluginName, func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
						return tc.plugin, nil
					}); err != nil {
						t.Fatalf("Failed to register test reserve plugin: %v", err)
					}
					reservePluginName = testReservePluginName
				}
				prof := newTestProfile(schedulerName, reservePluginName)
				profiles, err = newTestProfileMap(ctx, sharedSnap, registry, prof)
				if err != nil {
					t.Fatalf("newTestProfileMap failed: %v", err)
				}
			} else {
				profiles = newDummyProfileMap()
			}

			var internalCache cache.Cache
			if !tc.nilCache {
				internalCache = cache.New(ctx, nil, false)
				for _, n := range tc.nodeNames {
					internalCache.AddNode(logger, st.MakeNode().Name(n).Capacity(map[v1.ResourceName]string{
						v1.ResourceCPU:    "1",
						v1.ResourceMemory: "1Gi",
						v1.ResourcePods:   "110",
					}).Obj())
				}
			}
			var nodeName string
			if tc.pod != nil {
				nodeName = tc.pod.Spec.NodeName
			}
			state := New(internalCache, profiles, sharedSnap)
			var cycleState fwk.CycleState
			if !tc.nilCycleState {
				cycleState = framework.NewCycleState()
			}
			if tc.duplicateAssume {
				if err := state.AssumeAndReserve(ctx, tc.pod, cycleState, nodeName); err != nil {
					t.Fatalf("First AssumeAndReserve() unexpected error = %v", err)
				}
			}

			err = state.AssumeAndReserve(ctx, tc.pod, cycleState, nodeName)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AssumeAndReserve() error = %v, wantErr = %v", err, tc.wantErr)
			}

			if tc.plugin != nil {
				if tc.plugin.reserveCalled != tc.wantReserveCalled {
					t.Errorf("plugin.reserveCalled = %v, want %v", tc.plugin.reserveCalled, tc.wantReserveCalled)
				}
				if tc.plugin.unreserveCalled != tc.wantUnreserveCalled {
					t.Errorf("plugin.unreserveCalled = %v, want %v", tc.plugin.unreserveCalled, tc.wantUnreserveCalled)
				}
			}

			if tc.pod != nil && tc.pod.Spec.NodeName != "" && !tc.nilCache {
				isAssumed, _ := state.Cache.IsAssumedPod(tc.pod)
				if isAssumed != tc.wantAssumedInCache {
					t.Errorf("state.Cache.IsAssumedPod() = %v, want %v", isAssumed, tc.wantAssumedInCache)
				}
			}

			if tc.expectedSnapshot != nil {
				if err := state.SyncSnapshot(logger); err != nil {
					t.Fatalf("SyncSnapshot() error = %v", err)
				}
				ft.VerifySnapshot(t, sharedSnap, tc.expectedSnapshot)
			}
		})
	}
}

func TestClusterState_UnreserveAndForget(t *testing.T) {
	tests := []struct {
		name                 string
		nodeNames            []string
		assumePodFirst       bool
		boundPodFirst        bool
		pod                  *v1.Pod
		podToUnreserve       *v1.Pod
		plugin               *mockReservePlugin
		withCustomProfileMap bool
		profileSchedulerName string
		nilCache             bool
		nilProfiles          bool
		wantErr              bool
		wantUnreserveCalled  bool
		wantAssumedInCache   bool
		expectedSnapshot     map[string]sets.Set[string]
	}{
		{
			name:                 "successful unreserve and forget",
			nodeNames:            []string{"node1"},
			assumePodFirst:       true,
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			plugin:               &mockReservePlugin{},
			withCustomProfileMap: true,
			wantErr:              false,
			wantUnreserveCalled:  true,
			wantAssumedInCache:   false,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New[string]()},
		},
		{
			name:                 "unreserve and forget pod with node mismatch returns error",
			nodeNames:            []string{"node1", "node2"},
			assumePodFirst:       true,
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			podToUnreserve:       st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node2").Obj(),
			plugin:               &mockReservePlugin{},
			withCustomProfileMap: true,
			wantErr:              true,
			wantUnreserveCalled:  true,
			wantAssumedInCache:   true,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": sets.New[string]()},
		},
		{
			name:                 "unreserve and forget bound pod returns error",
			nodeNames:            []string{"node1"},
			boundPodFirst:        true,
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			plugin:               &mockReservePlugin{},
			withCustomProfileMap: true,
			wantErr:              true,
			wantUnreserveCalled:  false,
			wantAssumedInCache:   false,
			expectedSnapshot:     map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name:      "pod without node name fails",
			nodeNames: []string{"node1"},
			pod:       st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj(),
			wantErr:   true,
		},
		{
			name:      "nil pod fails",
			nodeNames: []string{"node1"},
			pod:       nil,
			wantErr:   true,
		},
		{
			name:                 "profile not found for pod scheduler name",
			nodeNames:            []string{"node1"},
			pod:                  st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").SchedulerName("custom-scheduler").Obj(),
			withCustomProfileMap: true,
			profileSchedulerName: v1.DefaultSchedulerName,
			wantErr:              true,
		},
		{
			name:     "nil cache returns error",
			pod:      st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			nilCache: true,
			wantErr:  true,
		},
		{
			name:        "nil profiles returns error",
			pod:         st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj(),
			nilProfiles: true,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			logger := klog.FromContext(ctx)
			sharedSnap := cache.NewEmptySnapshot()

			var profiles *upstreamsync.ProfileMap
			var err error
			if tc.nilProfiles {
				profiles = nil
			} else if tc.withCustomProfileMap {
				registry := plugins.NewInTreeRegistry()
				schedulerName := v1.DefaultSchedulerName
				if tc.profileSchedulerName != "" {
					schedulerName = tc.profileSchedulerName
				}
				var reservePluginName string
				if tc.plugin != nil {
					if err := registry.Register(testReservePluginName, func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
						return tc.plugin, nil
					}); err != nil {
						t.Fatalf("Failed to register test reserve plugin: %v", err)
					}
					reservePluginName = testReservePluginName
				}
				prof := newTestProfile(schedulerName, reservePluginName)
				profiles, err = newTestProfileMap(ctx, sharedSnap, registry, prof)
				if err != nil {
					t.Fatalf("newTestProfileMap failed: %v", err)
				}
			} else {
				profiles = newDummyProfileMap()
			}

			var internalCache cache.Cache
			if !tc.nilCache {
				internalCache = cache.New(ctx, nil, false)
				for _, n := range tc.nodeNames {
					internalCache.AddNode(logger, st.MakeNode().Name(n).Capacity(map[v1.ResourceName]string{
						v1.ResourceCPU:    "1",
						v1.ResourceMemory: "1Gi",
						v1.ResourcePods:   "110",
					}).Obj())
				}
			}
			state := New(internalCache, profiles, sharedSnap)
			cycleState := framework.NewCycleState()

			if tc.boundPodFirst && tc.pod != nil {
				if err := state.Cache.AddPod(logger, tc.pod); err != nil {
					t.Fatalf("Cache.AddPod() setup error = %v", err)
				}
			}

			if tc.assumePodFirst && tc.pod != nil {
				if err := state.AssumeAndReserve(ctx, tc.pod, cycleState, tc.pod.Spec.NodeName); err != nil {
					t.Fatalf("AssumeAndReserve() setup error = %v", err)
				}
				// Reset unreserveCalled from plugin since AssumeAndReserve might not have called unreserve,
				// but let's reset to clearly verify UnreserveAndForget.
				if tc.plugin != nil {
					tc.plugin.unreserveCalled = false
				}
			}

			podToUnreserve := tc.pod
			if tc.podToUnreserve != nil {
				podToUnreserve = tc.podToUnreserve
			}

			err = state.UnreserveAndForget(ctx, podToUnreserve)
			if (err != nil) != tc.wantErr {
				t.Fatalf("UnreserveAndForget() error = %v, wantErr = %v", err, tc.wantErr)
			}

			if tc.plugin != nil {
				if tc.plugin.unreserveCalled != tc.wantUnreserveCalled {
					t.Errorf("plugin.unreserveCalled = %v, want %v", tc.plugin.unreserveCalled, tc.wantUnreserveCalled)
				}
			}

			if tc.pod != nil && tc.pod.Spec.NodeName != "" && !tc.nilCache {
				isAssumed, _ := state.Cache.IsAssumedPod(tc.pod)
				if isAssumed != tc.wantAssumedInCache {
					t.Errorf("state.Cache.IsAssumedPod() = %v, want %v", isAssumed, tc.wantAssumedInCache)
				}
			}

			if tc.expectedSnapshot != nil {
				err := state.SyncSnapshot(logger)
				if err != nil {
					t.Fatalf("Snapshot() error = %v", err)
				}
				ft.VerifySnapshot(t, sharedSnap, tc.expectedSnapshot)
			}
		})
	}
}

func TestClusterState_AssumeAndReserve_Lifecycle(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)
	sharedSnap := cache.NewEmptySnapshot()

	plugin := &mockReservePlugin{}
	registry := plugins.NewInTreeRegistry()
	if err := registry.Register(testReservePluginName, func(ctx context.Context, obj runtime.Object, handle fwk.Handle) (fwk.Plugin, error) {
		return plugin, nil
	}); err != nil {
		t.Fatalf("Failed to register plugin: %v", err)
	}

	prof := newTestProfile(v1.DefaultSchedulerName, testReservePluginName)
	profiles, err := newTestProfileMap(ctx, sharedSnap, registry, prof)
	if err != nil {
		t.Fatalf("newTestProfileMap failed: %v", err)
	}

	internalCache := cache.New(ctx, nil, false)
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "2Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "2",
		v1.ResourceMemory: "2Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	internalCache.AddNode(logger, node1)
	internalCache.AddNode(logger, node2)

	state := New(internalCache, profiles, sharedSnap)

	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Node("node2").Obj()

	// 1. Initial snapshot has no pods
	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string](), "node2": sets.New[string]()})

	// 2. Assume and reserve pod1 on node1
	cycleState := framework.NewCycleState()
	if err := state.AssumeAndReserve(ctx, pod1, cycleState, pod1.Spec.NodeName); err != nil {
		t.Fatalf("AssumeAndReserve(pod1) error = %v", err)
	}
	isAssumed, _ := state.Cache.IsAssumedPod(pod1)
	if !isAssumed {
		t.Errorf("Expected pod1 to be assumed")
	}

	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": sets.New[string]()})

	// 3. Assume and reserve pod2 on node2
	cycleState2 := framework.NewCycleState()
	if err := state.AssumeAndReserve(ctx, pod2, cycleState2, pod2.Spec.NodeName); err != nil {
		t.Fatalf("AssumeAndReserve(pod2) error = %v", err)
	}
	isAssumed, _ = state.Cache.IsAssumedPod(pod2)
	if !isAssumed {
		t.Errorf("Expected pod2 to be assumed")
	}

	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": sets.New("pod2")})

	// 4. Unreserve and forget pod1
	if err := state.UnreserveAndForget(ctx, pod1); err != nil {
		t.Fatalf("UnreserveAndForget(pod1) error = %v", err)
	}
	isAssumed, _ = state.Cache.IsAssumedPod(pod1)
	if isAssumed {
		t.Errorf("Expected pod1 to not be assumed")
	}

	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string](), "node2": sets.New("pod2")})

	// 5. Unreserve and forget pod2
	if err := state.UnreserveAndForget(ctx, pod2); err != nil {
		t.Fatalf("UnreserveAndForget(pod2) error = %v", err)
	}
	isAssumed, _ = state.Cache.IsAssumedPod(pod2)
	if isAssumed {
		t.Errorf("Expected pod2 to not be assumed")
	}

	err = state.SyncSnapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string](), "node2": sets.New[string]()})
}
