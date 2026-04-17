package state

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			state := NewClusterState(cache.New(ctx, nil, false), nil)

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
