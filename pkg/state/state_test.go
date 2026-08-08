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

// recordingCache wraps a cache.Cache and records the *cache.Snapshot passed to
// each UpdateSnapshot call, so a test can tell an in-place incremental refresh
// (target == the shared snapshot) from an authoritative rebuild (target == a
// fresh snapshot).
type recordingCache struct {
	cache.Cache
	updateTargets []*cache.Snapshot
}

func (c *recordingCache) UpdateSnapshot(logger klog.Logger, snap *cache.Snapshot) error {
	c.updateTargets = append(c.updateTargets, snap)
	return c.Cache.UpdateSnapshot(logger, snap)
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
			state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

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
			csnap, err := state.Snapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
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
			state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

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
			csnap, err := state.Snapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
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
			state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}

			state.Cache.AddNode(logger, tc.nodeToAdd)

			ft.VerifySnapshot(t, sharedSnap, nil)
			csnap, err := state.Snapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
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
			state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}

			err := state.Cache.RemoveNode(logger, tc.nodeToRemove)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveNode() error = %v, wantErr = %v", err, tc.wantErr)
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			csnap, err := state.Snapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
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

			state := New(cache.New(ctx, nil, false), profiles, sharedSnap)

			for _, n := range tc.existingNodes {
				state.Cache.AddNode(logger, n)
			}
			for _, p := range tc.existingPods {
				if err := state.Cache.AddPod(logger, p); err != nil {
					t.Fatalf("Failed to add pod: %v", err)
				}
			}

			ft.VerifySnapshot(t, sharedSnap, nil)
			csnap, err := state.Snapshot(logger)
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
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
			csnap, err := state.Snapshot(klog.FromContext(t.Context()))
			if err != nil {
				t.Fatalf("Snapshot() error = %v", err)
			}
			if csnap == nil {
				t.Fatal("Expected ClusterSnapshot to be non-nil")
			}
		}
	}

	assertSnapshot := func(expected map[string]sets.Set[string]) action {
		return func(t *testing.T, state *ClusterState) {
			t.Helper()
			ft.VerifySnapshot(t, state.sharedSnap, expected)
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
			state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

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

func TestClusterStateSnapshotRestoresAuthoritativeStateAfterPreemption(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	node := st.MakeNode().Name("node1").Obj()
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

	state.Cache.AddNode(logger, node)
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}

	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})

	// PreemptPods mutates the shared snapshot in place and commits, so the victim
	// leaves the snapshot while the authoritative cache still holds it.
	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string]()})

	// The authoritative cache must still hold the victim: only the shared
	// snapshot diverged. This is the invariant the resync relies on, so pin it
	// directly rather than inferring it from the victim reappearing later.
	if cached, err := state.Cache.GetPod(victim); err != nil {
		t.Fatalf("authoritative cache lost the victim after PreemptPods: %v", err)
	} else if cached.UID != victim.UID {
		t.Fatalf("cached victim UID = %q, want %q", cached.UID, victim.UID)
	}

	// A fresh ClusterState snapshot is expected to resynchronize the shared
	// snapshot with the cache, restoring the victim.
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})

	// The rebuild clears the dirty flag, so later refreshes stay on the
	// incremental path. They must keep exactly one victim rather than dropping it
	// or compensating a second time.
	for i := 0; i < 2; i++ {
		if _, err := state.Snapshot(logger); err != nil {
			t.Fatalf("Snapshot() error = %v", err)
		}
		ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})
	}
}

func TestClusterStateSnapshotDoesNotLeaveEarlierMutationAfterAnotherNodeUpdate(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	node1 := st.MakeNode().Name("node1").Obj()
	node2 := st.MakeNode().Name("node2").Obj()
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)

	state.Cache.AddNode(logger, node1)
	state.Cache.AddNode(logger, node2)
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}

	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string](), "node2": sets.New[string]()})

	// An unrelated cache update bumps only node2's cache generation. The
	// incremental UpdateSnapshot copies node2, then stops at node1's unchanged
	// cache-side generation, so the earlier snapshot-side removal on node1 is
	// never undone.
	unrelated := st.MakePod().Name("unrelated").Namespace("default").UID("unrelated").Node("node2").Obj()
	if err := state.Cache.AddPod(logger, unrelated); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim"), "node2": sets.New("unrelated")})
}

func TestClusterStateSnapshotRestoresAfterCommittedTransaction(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	if err := csnap.Transaction(ctx, func() (snapshot.TransactionResult, error) {
		_, err := csnap.PreemptPods(ctx, []*v1.Pod{victim})
		return snapshot.Commit, err
	}); err != nil {
		t.Fatalf("Transaction() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string]()})

	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})
}

func TestClusterStateSnapshotAfterRevertedTransaction(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:  "4",
		v1.ResourcePods: "10",
	}).Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// The transaction reverts, so the removal is undone and the victim is back in
	// the snapshot before the refresh.
	if err := csnap.Transaction(ctx, func() (snapshot.TransactionResult, error) {
		_, err := csnap.PreemptPods(ctx, []*v1.Pod{victim})
		return snapshot.Revert, err
	}); err != nil {
		t.Fatalf("Transaction() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})

	// The refresh keeps exactly one victim: not dropped, not double-added. Check
	// the resource accounting too, since a double add-back would double the
	// request while still listing the pod only once.
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})
	ni, err := sharedSnap.Get("node1")
	if err != nil {
		t.Fatalf("Get(node1) error = %v", err)
	}
	if got := ni.GetRequested().GetMilliCPU(); got != 1000 {
		t.Fatalf("MilliCPU after revert + refresh = %d, want 1000 (double-counted or dropped)", got)
	}
}

func TestClusterStateSnapshotAfterPreemptUnpreempt(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:  "4",
		v1.ResourcePods: "10",
	}).Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).HostPort(8080).Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	handle, err := csnap.PreemptPods(ctx, []*v1.Pod{victim})
	if err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New[string]()})
	if _, err := csnap.Unpreempt(handle); err != nil {
		t.Fatalf("Unpreempt() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})

	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})
	// Restored exactly once: preempt subtracted the request and freed the port,
	// unpreempt added them back, and the refresh must not compound either.
	ni, err := sharedSnap.Get("node1")
	if err != nil {
		t.Fatalf("Get(node1) error = %v", err)
	}
	if got := ni.GetRequested().GetMilliCPU(); got != 1000 {
		t.Fatalf("MilliCPU after preempt/unpreempt + refresh = %d, want 1000", got)
	}
	if got := ni.GetUsedPorts().Len(); got != 1 {
		t.Fatalf("used ports after preempt/unpreempt + refresh = %d, want 1", got)
	}
}

func TestClusterStateSnapshotRestoresDerivedNodeState(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj())
	// A state-rich victim: the fix must restore every piece of scheduler-visible
	// state its removal unwound, not just the pod name. RemovePod rolls back
	// resource accounting, host ports, PVC references, and the anti-affinity
	// index, so a partial fix that only re-added the pod to Pods would pass a
	// name-only check while leaving the node mis-accounted.
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "1", v1.ResourceMemory: "1Gi"}).
		HostPort(8080).
		PVC("victim-pvc").
		PodAntiAffinityExists("team", "topology.kubernetes.io/zone", st.PodAntiAffinityWithRequiredReq).
		Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	ni, err := sharedSnap.Get("node1")
	if err != nil {
		t.Fatalf("Get(node1) error = %v", err)
	}
	// Pod identity, not just the name.
	pods := ni.GetPods()
	if len(pods) != 1 {
		t.Fatalf("restored pods = %d, want 1", len(pods))
	}
	if got := pods[0].GetPod().UID; got != victim.UID {
		t.Fatalf("restored pod UID = %q, want %q", got, victim.UID)
	}
	// Resource accounting.
	if got := ni.GetRequested().GetMilliCPU(); got != 1000 {
		t.Fatalf("restored MilliCPU = %d, want 1000 (derived state not restored)", got)
	}
	if got := ni.GetRequested().GetMemory(); got != 1<<30 {
		t.Fatalf("restored Memory = %d, want %d (1Gi)", got, int64(1<<30))
	}
	// Host ports, PVC references, and the per-node anti-affinity subset, by exact
	// key rather than count.
	if !ni.GetUsedPorts().CheckConflict("", "TCP", 8080) {
		t.Fatalf("restored used ports missing 8080/TCP")
	}
	if got := ni.GetPVCRefCounts()["default/victim-pvc"]; got != 1 {
		t.Fatalf("restored PVC ref count for default/victim-pvc = %d, want 1", got)
	}
	if got := len(ni.GetPodsWithRequiredAntiAffinity()); got != 1 {
		t.Fatalf("restored pods with required anti-affinity = %d, want 1", got)
	}
	// The snapshot-wide anti-affinity index, which a per-node RemovePod does not
	// maintain on its own: only the authoritative rebuild brings it back.
	withAntiAffinity, err := sharedSnap.NodeInfos().HavePodsWithRequiredAntiAffinityList()
	if err != nil {
		t.Fatalf("HavePodsWithRequiredAntiAffinityList() error = %v", err)
	}
	if len(withAntiAffinity) != 1 {
		t.Fatalf("nodes in anti-affinity index = %d, want 1", len(withAntiAffinity))
	}
	// Snapshot-wide PVC index.
	if !sharedSnap.StorageInfos().IsPVCUsedByPods("default/victim-pvc") {
		t.Fatalf("restored snapshot-wide PVC index missing default/victim-pvc")
	}
	ft.VerifySnapshot(t, sharedSnap, map[string]sets.Set[string]{"node1": sets.New("victim")})
}

// TestClusterStateSnapshotRefreshIsDirtyOnlyAfterDirectMutation pins the dirty
// flag contract: a plain cache refresh leaves the snapshot clean, and only an
// out-of-band direct mutation such as PreemptPods sets NeedsAuthoritativeRefresh.
// It checks the flag, not which snapshot the refresh targets;
// TestClusterStateSnapshotUsesFreshSnapshotOnlyOnRebuild pins the path itself.
func TestClusterStateSnapshotRefreshIsDirtyOnlyAfterDirectMutation(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}

	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("a clean snapshot must not need an authoritative refresh")
	}

	// An ordinary cache update advances the cache-side generation, so the
	// incremental refresh handles it; it must not set the dirty flag.
	if err := state.Cache.AddPod(logger, st.MakePod().Name("other").Namespace("default").UID("other").Node("node1").Obj()); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err = state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("an ordinary cache update must not need an authoritative refresh")
	}

	// A direct in-place mutation of the shared snapshot is the only thing that
	// forces the next refresh to rebuild.
	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	if !csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("a committed direct mutation must need an authoritative refresh")
	}
}

// TestClusterStateSnapshotResyncVisibleThroughFrameworkLister runs the resync
// assertions through a real framework's SnapshotSharedLister rather than a
// direct read of the shared snapshot. The rebuild must update the very snapshot
// the framework was constructed with; replacing that pointer would leave plugin
// listers looking at stale state even though a freshly returned snapshot is
// correct.
func TestClusterStateSnapshotResyncVisibleThroughFrameworkLister(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	fwk, err := frameworkruntime.NewFramework(ctx, plugins.NewInTreeRegistry(),
		&schedulerapi.KubeSchedulerProfile{SchedulerName: "default-scheduler"},
		frameworkruntime.WithSnapshotSharedLister(sharedSnap),
		frameworkruntime.WithInformerFactory(informerFactory),
	)
	if err != nil {
		t.Fatalf("NewFramework() error = %v", err)
	}
	profiles := &upstreamsync.ProfileMap{Map: profile.Map{"default-scheduler": fwk}}
	state := New(cache.New(ctx, nil, false), profiles, sharedSnap)

	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}

	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	ni, err := fwk.SnapshotSharedLister().NodeInfos().Get("node1")
	if err != nil {
		t.Fatalf("framework lister Get(node1) error = %v", err)
	}
	pods := ni.GetPods()
	if len(pods) != 1 || pods[0].GetPod().UID != victim.UID {
		t.Fatalf("framework lister sees %d pods, want the victim restored", len(pods))
	}
}

// TestClusterStateSnapshotUsesFreshSnapshotOnlyOnRebuild is the real fast-path
// oracle: it records which *cache.Snapshot each refresh targets. Clean refreshes
// must update the shared snapshot in place; only a refresh after a direct
// mutation may rebuild into a fresh snapshot. An implementation that always
// rebuilt would fail the clean-path checks, and one that never rebuilt would
// fail the dirty-path check.
func TestClusterStateSnapshotUsesFreshSnapshotOnlyOnRebuild(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	rc := &recordingCache{Cache: cache.New(ctx, nil, false)}
	state := New(rc, newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	lastTarget := func() *cache.Snapshot { return rc.updateTargets[len(rc.updateTargets)-1] }

	_, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if lastTarget() != sharedSnap {
		t.Fatalf("clean refresh did not update the shared snapshot in place")
	}

	if err := state.Cache.AddPod(logger, st.MakePod().Name("other").Namespace("default").UID("other").Node("node1").Obj()); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	// Use the latest wrapper so the mutation lands on the snapshot that
	// ClusterState will consult on the next refresh.
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if lastTarget() != sharedSnap {
		t.Fatalf("ordinary cache update left the incremental path")
	}

	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim}); err != nil {
		t.Fatalf("PreemptPods() error = %v", err)
	}
	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if lastTarget() == sharedSnap {
		t.Fatalf("dirty refresh updated the shared snapshot in place instead of a fresh one")
	}
}

// TestClusterStateSnapshotRebuildsAfterRevertedTransactionWithStalePod covers the
// case that makes trusting the undo unsafe. The undo re-adds the caller's pod
// object, not the one removed, so a stale object with the victim's UID but a
// different request corrupts the accounting on revert. The next refresh must
// rebuild from the authoritative cache rather than trust that undo.
func TestClusterStateSnapshotRebuildsAfterRevertedTransactionWithStalePod(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:  "8",
		v1.ResourcePods: "10",
	}).Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// Same UID as the authoritative victim, different request. RemovePod matches
	// by UID and subtracts the real 1 CPU; the undo re-adds this stale 2 CPU.
	stale := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj()
	if err := csnap.Transaction(ctx, func() (snapshot.TransactionResult, error) {
		_, err := csnap.PreemptPods(ctx, []*v1.Pod{stale})
		return snapshot.Revert, err
	}); err != nil {
		t.Fatalf("Transaction() error = %v", err)
	}
	if !csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("a reverted preemption must still request an authoritative refresh")
	}

	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ni, err := sharedSnap.Get("node1")
	if err != nil {
		t.Fatalf("Get(node1) error = %v", err)
	}
	if got := ni.GetRequested().GetMilliCPU(); got != 1000 {
		t.Fatalf("MilliCPU after reverted stale preemption = %d, want 1000 (authoritative 1 CPU, not the stale 2)", got)
	}
}

// TestClusterStateSnapshotRebuildsAfterPartialPreemptionFailure covers the error
// path: one removal succeeds, the next fails, and the call rolls back. The
// mutation still happened, so the flag must be set despite the returned error.
func TestClusterStateSnapshotRebuildsAfterPartialPreemptionFailure(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:  "8",
		v1.ResourcePods: "10",
	}).Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").
		Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj()
	if err := state.Cache.AddPod(logger, victim); err != nil {
		t.Fatalf("AddPod() error = %v", err)
	}
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// The second pod is not in the snapshot, so its removal fails after the first
	// succeeds and the call rolls back with an error.
	missing := st.MakePod().Name("missing").Namespace("default").UID("missing").Node("node1").Obj()
	if _, err := csnap.PreemptPods(ctx, []*v1.Pod{victim, missing}); err == nil {
		t.Fatalf("PreemptPods() with a missing pod: want error, got nil")
	}
	if !csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("a rolled-back partial preemption must still request an authoritative refresh")
	}

	if _, err := state.Snapshot(logger); err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	ni, err := sharedSnap.Get("node1")
	if err != nil {
		t.Fatalf("Get(node1) error = %v", err)
	}
	if got := ni.GetRequested().GetMilliCPU(); got != 1000 {
		t.Fatalf("MilliCPU after rolled-back partial preemption = %d, want 1000", got)
	}
}

// TestClusterStateSnapshotEmptyPreemptionStaysClean checks that a preemption that
// removes nothing does not force a rebuild.
func TestClusterStateSnapshotEmptyPreemptionStaysClean(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	csnap, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if _, err := csnap.PreemptPods(ctx, nil); err != nil {
		t.Fatalf("PreemptPods(nil) error = %v", err)
	}
	if csnap.NeedsAuthoritativeRefresh() {
		t.Fatalf("an empty preemption must not request an authoritative refresh")
	}
}
