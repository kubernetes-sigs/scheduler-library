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
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/klog/v2"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	plugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/profile"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/framework"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func init() {
	framework.InitMetricsOnce()
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

	placement, err := csnap.MakePlacement(sets.New("node1"))
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
