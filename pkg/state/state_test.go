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
	"fmt"
	"sync"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	plugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

func init() {
	metrics.Register()
}

func TestClusterState_AddPod(t *testing.T) {
	tests := []struct {
		name         string
		existingPods []*v1.Pod
		podToAdd     *v1.Pod
		expectCount  int
	}{
		{
			name: "add unassigned pod",
			podToAdd: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
			},
			expectCount: 1,
		},
		{
			name: "add assigned pod",
			podToAdd: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
				Spec:       v1.PodSpec{NodeName: "node1"},
			},
			expectCount: 1,
		},
		{
			name: "add duplicate pod",
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"}},
			},
			podToAdd: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
			},
			expectCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			state := NewClusterState(cache.New(ctx, nil, false), nil)
			cs := state.(*clusterState)

			for _, p := range tc.existingPods {
				state.AddPod(ctx, p)
			}

			state.AddPod(ctx, tc.podToAdd)

			count, err := cs.internalCache.PodCount()
			if err != nil {
				t.Fatalf("Failed to get pod count: %v", err)
			}
			if count != tc.expectCount {
				t.Errorf("Expected pod count %d, got %d", tc.expectCount, count)
			}
		})
	}
}

func TestClusterState_RemovePod(t *testing.T) {
	tests := []struct {
		name         string
		existingPods []*v1.Pod
		podToRemove  *v1.Pod
		expectCount  int
	}{
		{
			name: "remove existing pod",
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"}},
			},
			podToRemove: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
			},
			expectCount: 0,
		},
		{
			name: "remove non-existent pod",
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"}},
			},
			podToRemove: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "pod2", Namespace: "default", UID: "uid2"},
			},
			expectCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			state := NewClusterState(cache.New(ctx, nil, false), nil)
			cs := state.(*clusterState)

			for _, p := range tc.existingPods {
				state.AddPod(ctx, p)
			}

			state.RemovePod(ctx, tc.podToRemove)

			count, err := cs.internalCache.PodCount()
			if err != nil {
				t.Fatalf("Failed to get pod count: %v", err)
			}
			if count != tc.expectCount {
				t.Errorf("Expected pod count %d, got %d", tc.expectCount, count)
			}
		})
	}
}

func TestClusterState_AddNode(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		nodeToAdd     *v1.Node
		expectCount   int
	}{
		{
			name: "add valid node",
			nodeToAdd: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			expectCount: 1,
		},
		{
			name: "add duplicate node",
			existingNodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			nodeToAdd: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			expectCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			state := NewClusterState(cache.New(ctx, nil, false), nil)
			cs := state.(*clusterState)

			for _, n := range tc.existingNodes {
				state.AddNode(ctx, n)
			}

			state.AddNode(ctx, tc.nodeToAdd)

			count := cs.internalCache.NodeCount()
			if count != tc.expectCount {
				t.Errorf("Expected node count %d, got %d", tc.expectCount, count)
			}
		})
	}
}

func TestClusterState_RemoveNode(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		nodeToRemove  *v1.Node
		expectCount   int
	}{
		{
			name: "remove existing node",
			existingNodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			nodeToRemove: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node1"},
			},
			expectCount: 0,
		},
		{
			name: "remove non-existent node",
			existingNodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			nodeToRemove: &v1.Node{
				ObjectMeta: metav1.ObjectMeta{Name: "node2"},
			},
			expectCount: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			state := NewClusterState(cache.New(ctx, nil, false), nil)
			cs := state.(*clusterState)

			for _, n := range tc.existingNodes {
				state.AddNode(ctx, n)
			}

			state.RemoveNode(ctx, tc.nodeToRemove)

			count := cs.internalCache.NodeCount()
			if count != tc.expectCount {
				t.Errorf("Expected node count %d, got %d", tc.expectCount, count)
			}
		})
	}
}

func TestClusterState_Snapshot(t *testing.T) {
	tests := []struct {
		name          string
		existingNodes []*v1.Node
		existingPods  []*v1.Pod
		hasFramework  bool
	}{
		{
			name: "empty snapshot",
		},
		{
			name: "snapshot with data",
			existingNodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			existingPods: []*v1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"}},
			},
		},
		{
			name: "snapshot in sync with framework snapshot",
			existingNodes: []*v1.Node{
				{ObjectMeta: metav1.ObjectMeta{Name: "node1"}},
			},
			existingPods: []*v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
					Spec:       v1.PodSpec{NodeName: "node1"},
				},
			},
			hasFramework: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			var frameworks map[string]schedulerframework.Framework
			var sharedSnap *cache.Snapshot
			if tc.hasFramework {
				sharedSnap = cache.NewEmptySnapshot()
				informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
				registry := plugins.NewInTreeRegistry()
				profile := schedulerapi.KubeSchedulerProfile{
					SchedulerName: "default-scheduler",
				}
				fwk, err := frameworkruntime.NewFramework(ctx, registry, &profile,
					frameworkruntime.WithSnapshotSharedLister(sharedSnap),
					frameworkruntime.WithInformerFactory(informerFactory),
				)
				if err != nil {
					t.Fatalf("Failed to create framework: %v", err)
				}
				frameworks = map[string]schedulerframework.Framework{
					"default-scheduler": fwk,
				}
			}

			state := NewClusterState(cache.New(ctx, nil, false), frameworks)

			for _, n := range tc.existingNodes {
				state.AddNode(ctx, n)
			}
			for _, p := range tc.existingPods {
				state.AddPod(ctx, p)
			}

			snap := state.Snapshot(ctx)
			if snap == nil {
				t.Fatal("Expected snapshot to be non-nil")
			}

			if tc.hasFramework {
				for _, n := range tc.existingNodes {
					nodeInfo, err := sharedSnap.NodeInfos().Get(n.Name)
					if err != nil {
						t.Fatalf("Failed to get node %s from shared framework snapshot: %v", n.Name, err)
					}
					if nodeInfo == nil {
						t.Fatalf("Expected nodeInfo for node %s to be non-nil", n.Name)
					}

					expectedPods := make(map[string]bool)
					for _, p := range tc.existingPods {
						if p.Spec.NodeName == n.Name {
							expectedPods[p.Name] = true
						}
					}

					pods := nodeInfo.GetPods()
					if len(pods) != len(expectedPods) {
						t.Fatalf("Expected %d pods on node %s in shared framework snapshot, got %d", len(expectedPods), n.Name, len(pods))
					}

					for _, podInfo := range pods {
						podName := podInfo.GetPod().Name
						if !expectedPods[podName] {
							t.Fatalf("Unexpected pod %s on node %s in shared framework snapshot", podName, n.Name)
						}
					}
				}
			}
		})
	}
}

func TestClusterState_ComplexScenarios(t *testing.T) {
	t.Run("pod assigned to non-existent node", func(t *testing.T) {
		ctx := t.Context()
		state := NewClusterState(cache.New(ctx, nil, false), nil)
		cs := state.(*clusterState)

		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
			Spec:       v1.PodSpec{NodeName: "node1"},
		}

		// Add pod first
		state.AddPod(ctx, pod)

		// Verify pod count
		pCount, err := cs.internalCache.PodCount()
		if err != nil {
			t.Fatal(err)
		}
		if pCount != 1 {
			t.Errorf("Expected pod count 1, got %d", pCount)
		}

		// Add node later
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		}
		state.AddNode(ctx, node)

		// Verify node count
		nCount := cs.internalCache.NodeCount()
		if nCount != 1 {
			t.Errorf("Expected node count 1, got %d", nCount)
		}
	})

	t.Run("node removal with pods", func(t *testing.T) {
		ctx := t.Context()
		state := NewClusterState(cache.New(ctx, nil, false), nil)
		cs := state.(*clusterState)

		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node1"},
		}
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod1", Namespace: "default", UID: "uid1"},
			Spec:       v1.PodSpec{NodeName: "node1"},
		}

		state.AddNode(ctx, node)
		state.AddPod(ctx, pod)

		// Remove node
		state.RemoveNode(ctx, node)

		// Verify node count is still 1 (ghost node) because pod still exists
		nCount := cs.internalCache.NodeCount()
		if nCount != 1 {
			t.Errorf("Expected node count 1 (ghost node), got %d", nCount)
		}

		// Verify pod count is still 1
		pCount, err := cs.internalCache.PodCount()
		if err != nil {
			t.Fatal(err)
		}
		if pCount != 1 {
			t.Errorf("Expected pod count 1, got %d", pCount)
		}

		// Now remove the pod
		state.RemovePod(ctx, pod)

		// Verify node count becomes 0 after pod is removed
		nCount = cs.internalCache.NodeCount()
		if nCount != 0 {
			t.Errorf("Expected node count 0 after pod removal, got %d", nCount)
		}
	})
}

func TestClusterState_ConcurrentAccess(t *testing.T) {
	ctx := t.Context()
	state := NewClusterState(cache.New(ctx, nil, false), nil)

	const numGoroutines = 20
	const numOperations = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numOperations; j++ {
				pod := &v1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("pod-%d-%d", id, j),
						Namespace: "default",
						UID:       types.UID(fmt.Sprintf("uid-%d-%d", id, j)),
					},
				}
				node := &v1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("node-%d-%d", id, j)},
				}

				state.AddNode(ctx, node)
				state.AddPod(ctx, pod)
				state.Snapshot(ctx)
				state.RemovePod(ctx, pod)
				state.RemoveNode(ctx, node)
			}
		}(i)
	}

	wg.Wait()
}
