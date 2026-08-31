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

package snapshot

import (
	"math"
	"slices"
	"testing"

	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

var (
	byPodName = func(podName string) func(p framework.PodInfo) bool {
		return func(p framework.PodInfo) bool {
			return p.GetPod().Name == podName
		}
	}
)

func newSnapshot(pods []*v1.Pod, nodes []*v1.Node) *upstreamsync.MutatingSnapshot {
	return upstreamsync.NewMutatingSnapshot(cache.NewSnapshot(pods, nodes))
}

func TestAddPodToNode(t *testing.T) {
	ctx := t.Context()
	nodeName := "node1"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			UID:       "uid1",
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
		},
	}

	tests := []struct {
		name       string
		nodes      []*v1.Node
		pod        *v1.Pod
		targetNode string
		expectErr  bool
	}{
		{
			name: "node exists, pod is added successfully",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				},
			},
			pod:        pod,
			targetNode: nodeName,
			expectErr:  false,
		},
		{
			name:       "node does not exist",
			nodes:      nil,
			pod:        pod,
			targetNode: "non-existent-node",
			expectErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := newSnapshot(nil, tc.nodes)

			revertFn, err := addPodToNode(ctx, snapshot, tc.pod, tc.targetNode)
			if (err != nil) != tc.expectErr {
				t.Fatalf("unexpected error state: %v, expectErr: %v", err, tc.expectErr)
			}
			if tc.expectErr {
				if revertFn != nil {
					t.Error("expected revertFn to be nil")
				}
				return
			}
			if revertFn == nil {
				t.Fatal("expected revertFn to be non-nil")
			}

			nodeInfo, err := snapshot.Get(tc.targetNode)
			if err != nil {
				t.Fatalf("unexpected error getting node: %v", err)
			}

			if !slices.ContainsFunc(nodeInfo.GetPods(), byPodName(tc.pod.Name)) {
				t.Errorf("expected pod %s to be added to %s", tc.pod.Name, tc.targetNode)
			}

			// Run revert function
			revertFn()

			nodeInfo, err = snapshot.Get(tc.targetNode)
			if err != nil {
				t.Fatalf("unexpected error getting node: %v", err)
			}

			if slices.ContainsFunc(nodeInfo.GetPods(), byPodName(tc.pod.Name)) {
				t.Errorf("expected pod %s to be removed from %s after revert", tc.pod.Name, tc.targetNode)
			}
		})
	}
}

func TestRemovePodFromNode(t *testing.T) {
	ctx := t.Context()
	nodeName := "node1"
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			UID:       "uid1",
		},
		Spec: v1.PodSpec{
			NodeName: nodeName,
		},
	}

	tests := []struct {
		name       string
		nodes      []*v1.Node
		initPods   []*v1.Pod
		pod        *v1.Pod
		targetNode string
		expectErr  bool
	}{
		{
			name: "node and pod exist, pod is removed",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				},
			},
			initPods:   []*v1.Pod{pod},
			pod:        pod,
			targetNode: nodeName,
			expectErr:  false,
		},
		{
			name:       "node does not exist",
			nodes:      nil,
			initPods:   nil,
			pod:        pod,
			targetNode: "non-existent-node",
			expectErr:  true,
		},
		{
			name: "pod is not scheduled on node",
			nodes: []*v1.Node{
				{
					ObjectMeta: metav1.ObjectMeta{Name: nodeName},
				},
			},
			initPods:   nil,
			pod:        pod,
			targetNode: nodeName,
			expectErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := newSnapshot(tc.initPods, tc.nodes)
			pod = tc.pod.DeepCopy()
			pod.Spec.NodeName = tc.targetNode

			revertFn, err := removePodFromNode(ctx, snapshot, pod)
			if (err != nil) != tc.expectErr {
				t.Fatalf("unexpected error state: %v, expectErr: %v", err, tc.expectErr)
			}
			if tc.expectErr {
				if revertFn != nil {
					t.Error("expected revertFn to be nil")
				}
				return
			}
			if revertFn == nil {
				t.Fatal("expected revertFn to be non-nil")
			}

			nodeInfo, err := snapshot.Get(tc.targetNode)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if slices.ContainsFunc(nodeInfo.GetPods(), byPodName(pod.Name)) {
				t.Errorf("expected pod %s to be removed from %s", pod.Name, tc.targetNode)
			}

			// Run revert function
			revertFn()

			nodeInfo, err = snapshot.Get(tc.targetNode)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.ContainsFunc(nodeInfo.GetPods(), byPodName(pod.Name)) {
				t.Errorf("expected pod %s, got %s", pod.Name, nodeInfo.GetPods()[0].GetPod().Name)
			}
		})
	}
}

func TestCreatePodFromTemplate(t *testing.T) {
	tests := []struct {
		name            string
		template        *v1.PodTemplateSpec
		index           int
		expectedPrefix  string
		expectedNS      string
		checkLabels     func(t *testing.T, labels map[string]string)
		checkContainers func(t *testing.T, containers []v1.Container)
	}{
		{
			name: "complete template",
			template: &v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-app",
					Namespace: "custom-ns",
					Labels:    map[string]string{"foo": "bar"},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{Name: "nginx", Image: "nginx"},
					},
				},
			},
			index:          3,
			expectedPrefix: "my-app-3-",
			expectedNS:     "custom-ns",
			checkLabels: func(t *testing.T, labels map[string]string) {
				if labels["foo"] != "bar" {
					t.Errorf("expected label foo=bar, got %q", labels["foo"])
				}
			},
			checkContainers: func(t *testing.T, containers []v1.Container) {
				if len(containers) != 1 || containers[0].Name != "nginx" {
					t.Error("pod spec container does not match template")
				}
			},
		},
		{
			name:           "empty template name and namespace defaults",
			template:       &v1.PodTemplateSpec{},
			index:          0,
			expectedPrefix: "templated-pod-0-",
			expectedNS:     "default",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := createPodFromTemplate(tc.template, tc.index)
			if pod == nil {
				t.Fatal("expected pod to be non-nil")
			}
			if len(pod.Name) <= len(tc.expectedPrefix) || pod.Name[:len(tc.expectedPrefix)] != tc.expectedPrefix {
				t.Errorf("expected pod name to start with %q, got %q", tc.expectedPrefix, pod.Name)
			}
			if pod.Namespace != tc.expectedNS {
				t.Errorf("expected namespace %q, got %q", tc.expectedNS, pod.Namespace)
			}
			if pod.UID == "" {
				t.Error("expected non-empty UID")
			}
			if tc.checkLabels != nil {
				tc.checkLabels(t, pod.Labels)
			}
			if tc.checkContainers != nil {
				tc.checkContainers(t, pod.Spec.Containers)
			}
		})
	}
}

func TestScheduleOnePod(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name          string
		nodes         []*v1.Node
		pod           *v1.Pod
		candidate     string
		expectSuccess bool
		expectErr     bool
	}{
		{
			name: "success - pod is scheduled",
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    "10",
					v1.ResourceMemory: "10Gi",
					v1.ResourcePods:   "110",
				}).Obj(),
			},
			pod:           st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").SchedulerName(v1.DefaultSchedulerName).Req(map[v1.ResourceName]string{v1.ResourceCPU: "1"}).Obj(),
			candidate:     "node1",
			expectSuccess: true,
			expectErr:     false,
		},
		{
			name: "unschedulable pod",
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    "1",
					v1.ResourceMemory: "10Gi",
					v1.ResourcePods:   "110",
				}).Obj(),
			},
			pod:           st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").SchedulerName(v1.DefaultSchedulerName).Req(map[v1.ResourceName]string{v1.ResourceCPU: "2"}).Obj(),
			candidate:     "node1",
			expectSuccess: false,
			expectErr:     false,
		},
		{
			name:          "error - framework not found",
			nodes:         []*v1.Node{st.MakeNode().Name("node1").Obj()},
			pod:           st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").SchedulerName("non-existent-scheduler-profile").Obj(),
			candidate:     "node1",
			expectSuccess: false,
			expectErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs, snap, _ := setupSnapshotTest(t, ctx, tc.nodes, nil)

			placement, err := cs.MakePlacement([]string{tc.candidate})
			if err != nil {
				t.Fatalf("MakePlacement failed: %v", err)
			}
			if err := snap.AssumePlacement(placement); err != nil {
				t.Fatalf("AssumePlacement failed: %v", err)
			}
			defer snap.ForgetPlacement()

			algRes, revertFn, err := scheduleOnePod(ctx, cs.profiles, upstreamsync.NewScheduler(snap, 0, 0, math.MaxInt32, nil), tc.pod)
			if (err != nil) != tc.expectErr {
				t.Fatalf("unexpected error state: %v, expectErr: %v", err, tc.expectErr)
			}
			if tc.expectErr {
				return
			}
			if algRes == nil {
				t.Fatal("expected algRes to be non-nil")
			}
			if algRes.Status.IsSuccess() != tc.expectSuccess {
				t.Fatalf("expected scheduling success %v, got %v", tc.expectSuccess, algRes.Status.IsSuccess())
			}

			// scheduleOnePod leaves the pod untouched; reflecting the result on it is up to the caller.
			if tc.pod.Spec.NodeName != "" {
				t.Errorf("expected pod.Spec.NodeName to remain empty, got %q", tc.pod.Spec.NodeName)
			}

			if tc.expectSuccess {
				if algRes.ScheduleResult.SuggestedHost != tc.candidate {
					t.Errorf("expected SuggestedHost to be %q, got %q", tc.candidate, algRes.ScheduleResult.SuggestedHost)
				}
				if revertFn == nil {
					t.Fatal("expected revertFn to be non-nil")
				}

				// Revert the scheduling
				revertFn()

				nodeInfo, err := snap.Get(tc.candidate)
				if err != nil {
					t.Fatalf("unexpected error getting node: %v", err)
				}
				if len(nodeInfo.GetPods()) != 0 {
					t.Errorf("expected 0 pods on node after revert, got %d", len(nodeInfo.GetPods()))
				}
			} else {
				if revertFn != nil {
					t.Error("expected revertFn to be nil")
				}
			}
		})
	}
}

func setupSnapshotTest(t *testing.T, ctx context.Context, nodes []*v1.Node, pods []*v1.Pod) (*ClusterSnapshot, *cache.Snapshot, *upstreamsync.ProfileMap) {
	profiles, snap, err := ft.SetupSnapshotTest(ctx, pods, nodes)
	if err != nil {
		t.Fatalf("Failed to set up snapshot: %v", err)
	}
	return New(snap, profiles), snap, profiles
}
