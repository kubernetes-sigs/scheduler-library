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

package upstreamsync_test

import (
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

func makeNode(name string) *v1.Node {
	return st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
}

func makeScheduledPod(name, nodeName string) *v1.Pod {
	return st.MakePod().Name(name).Namespace("default").UID(name).Node(nodeName).Obj()
}

func podNamesOnNode(t *testing.T, snapshot *upstreamsync.MutatingSnapshot, nodeName string) []string {
	t.Helper()
	nodeInfo, err := snapshot.Get(nodeName)
	if err != nil {
		t.Fatalf("unexpected error getting node %s: %v", nodeName, err)
	}
	names := make([]string, 0, len(nodeInfo.GetPods()))
	for _, podInfo := range nodeInfo.GetPods() {
		names = append(names, podInfo.GetPod().Name)
	}
	slices.Sort(names)
	return names
}

func mustPodInfo(t *testing.T, pod *v1.Pod) *framework.PodInfo {
	t.Helper()
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		t.Fatalf("unexpected error creating pod info for %s: %v", pod.Name, err)
	}
	return podInfo
}

// TestMutatingSnapshotRestoreState checks that RestoreState reverts a mix of AddPod and RemovePod
// calls and that a second RestoreState does nothing.
func TestMutatingSnapshotRestoreState(t *testing.T) {
	logger := klog.Background()
	nodeName := "node1"
	node := makeNode(nodeName)
	pod1 := makeScheduledPod("pod1", nodeName)
	pod2 := makeScheduledPod("pod2", nodeName)
	pod3 := makeScheduledPod("pod3", nodeName)
	pod4 := makeScheduledPod("pod4", nodeName)

	snapshot := upstreamsync.NewMutatingSnapshot(cache.NewSnapshot([]*v1.Pod{pod1, pod2}, []*v1.Node{node}))

	if err := snapshot.AddPod(logger, mustPodInfo(t, pod3)); err != nil {
		t.Fatalf("unexpected error adding pod3: %v", err)
	}
	if err := snapshot.RemovePod(logger, mustPodInfo(t, pod1)); err != nil {
		t.Fatalf("unexpected error removing pod1: %v", err)
	}
	if err := snapshot.AddPod(logger, mustPodInfo(t, pod4)); err != nil {
		t.Fatalf("unexpected error adding pod4: %v", err)
	}

	if err := snapshot.RestoreState(); err != nil {
		t.Fatalf("unexpected error restoring state: %v", err)
	}

	if got, want := podNamesOnNode(t, snapshot, nodeName), []string{"pod1", "pod2"}; !slices.Equal(got, want) {
		t.Errorf("expected pods %v on %s after restore, got %v", want, nodeName, got)
	}

	// A second restore must be a no-op.
	if err := snapshot.RestoreState(); err != nil {
		t.Fatalf("unexpected error on second restore: %v", err)
	}
	if got, want := podNamesOnNode(t, snapshot, nodeName), []string{"pod1", "pod2"}; !slices.Equal(got, want) {
		t.Errorf("expected pods %v on %s after second restore, got %v", want, nodeName, got)
	}
}

// TestMutatingSnapshotRestoreStateMovedPod checks that a pod removed from one node and added to
// another is put back on its original node.
func TestMutatingSnapshotRestoreStateMovedPod(t *testing.T) {
	logger := klog.Background()
	pod := makeScheduledPod("pod1", "node1")
	snapshot := upstreamsync.NewMutatingSnapshot(cache.NewSnapshot([]*v1.Pod{pod}, []*v1.Node{makeNode("node1"), makeNode("node2")}))

	if err := snapshot.RemovePod(logger, mustPodInfo(t, pod)); err != nil {
		t.Fatalf("unexpected error removing pod1: %v", err)
	}
	moved := pod.DeepCopy()
	moved.Spec.NodeName = "node2"
	if err := snapshot.AddPod(logger, mustPodInfo(t, moved)); err != nil {
		t.Fatalf("unexpected error adding pod1 to node2: %v", err)
	}

	if err := snapshot.RestoreState(); err != nil {
		t.Fatalf("unexpected error restoring state: %v", err)
	}

	if got, want := podNamesOnNode(t, snapshot, "node1"), []string{"pod1"}; !slices.Equal(got, want) {
		t.Errorf("expected pods %v on node1 after restore, got %v", want, got)
	}
	if got := podNamesOnNode(t, snapshot, "node2"); len(got) != 0 {
		t.Errorf("expected no pods on node2 after restore, got %v", got)
	}
}

// TestMutatingSnapshotRestoreStateReturnsError checks that RestoreState reports a failed inverse
// operation instead of silently dropping it.
func TestMutatingSnapshotRestoreStateReturnsError(t *testing.T) {
	logger := klog.Background()
	nodeName := "node1"
	node := makeNode(nodeName)
	pod1 := makeScheduledPod("pod1", nodeName)

	snapshot := upstreamsync.NewMutatingSnapshot(cache.NewSnapshot([]*v1.Pod{pod1}, []*v1.Node{node}))

	if err := snapshot.RemovePod(logger, mustPodInfo(t, pod1)); err != nil {
		t.Fatalf("unexpected error removing pod1: %v", err)
	}

	// Drop the node from the underlying snapshot, so that re-adding the pod fails.
	nodeInfo, err := snapshot.Get(nodeName)
	if err != nil {
		t.Fatalf("unexpected error getting node %s: %v", nodeName, err)
	}
	nodeInfo.(*framework.NodeInfo).RemoveNode()

	if err := snapshot.RestoreState(); err == nil {
		t.Error("expected an error from RestoreState when the inverse operation fails, got nil")
	}
}
