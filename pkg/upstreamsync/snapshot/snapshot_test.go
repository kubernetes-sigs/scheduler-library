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
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

type stepContext struct {
	ctx     context.Context
	cs      *ClusterSnapshot
	snap    *cache.Snapshot
	pods    map[string]*v1.Pod
	handles map[string]*Unpreemption
}

type stepFn func(t *testing.T, sc *stepContext)

func preempt(handleKey string, podNames ...string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		var pods []*v1.Pod
		for _, name := range podNames {
			p, ok := sc.pods[name]
			if !ok {
				t.Fatalf("preempt: pod %q not found in stepContext", name)
			}
			pods = append(pods, p)
		}
		handle, err := sc.cs.PreemptPods(sc.ctx, pods)
		if err != nil {
			t.Fatalf("PreemptPods(%v) unexpected error: %v", podNames, err)
		}
		sc.handles[handleKey] = handle
	}
}

func preemptErr(wantErr string, podNames ...string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		var pods []*v1.Pod
		for _, name := range podNames {
			p, ok := sc.pods[name]
			if !ok {
				t.Fatalf("preemptErr: pod %q not found in stepContext", name)
			}
			pods = append(pods, p)
		}
		_, err := sc.cs.PreemptPods(sc.ctx, pods)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("PreemptPods(%v) expected error containing %q, got: %v", podNames, wantErr, err)
		}
	}
}

func unpreempt(handleKey string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		handle := sc.handles[handleKey]
		_, err := sc.cs.Unpreempt(handle)
		if err != nil {
			t.Fatalf("Unpreempt(%q) unexpected error: %v", handleKey, err)
		}
	}
}

func unpreemptErr(handleKey string, wantErr string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		var handle *Unpreemption
		if handleKey != "nil" {
			handle = sc.handles[handleKey]
		}
		_, err := sc.cs.Unpreempt(handle)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Unpreempt(%q) expected error containing %q, got: %v", handleKey, wantErr, err)
		}
	}
}

func schedule(podNames []string, candidateNodes []string, opts SchedulePodsOptions) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		var pods []*v1.Pod
		for _, name := range podNames {
			p, ok := sc.pods[name]
			if !ok {
				t.Fatalf("schedule: pod %q not found in stepContext", name)
			}
			pods = append(pods, p)
		}
		placement, err := sc.cs.MakePlacement(candidateNodes)
		if err != nil {
			t.Fatalf("schedule: MakePlacement failed: %v", err)
		}
		_, err = sc.cs.SchedulePods(sc.ctx, pods, placement, opts)
		if err != nil {
			t.Fatalf("SchedulePods(%v) unexpected error: %v", podNames, err)
		}
	}
}

func scheduleWorkload(podNames []string, opts ScheduleWorkloadOptions, wantSuccess bool) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		var pods []*v1.Pod
		for _, name := range podNames {
			p, ok := sc.pods[name]
			if !ok {
				t.Fatalf("scheduleWorkload: pod %q not found in stepContext", name)
			}
			pods = append(pods, p)
		}
		result := sc.cs.ScheduleWorkload(sc.ctx, pods, opts)
		if wantSuccess {
			if !result.Status.IsSuccess() {
				t.Fatalf("ScheduleWorkload(%v) unexpected error: %v", podNames, result.Status.AsError())
			}
			if !result.Status.IsSuccess() {
				t.Fatalf("ScheduleWorkload(%v) unexpected workload failure status: %v", podNames, result.Status)
			}
			for i, podRes := range result.PodResults {
				if !podRes.Status.IsSuccess() {
					t.Fatalf("ScheduleWorkload(%v) podResult[%d] unexpected failure status: %v", podNames, i, podRes.Status)
				}
			}
		} else if len(result.PodResults) > 0 && result.Status.IsSuccess() {
			t.Fatalf("ScheduleWorkload(%v) expected failure status, but workload succeeded: %+v", podNames, result)
		}
	}
}

func canSchedule(podName string, candidateNodes []string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		p, ok := sc.pods[podName]
		if !ok {
			t.Fatalf("canSchedule: pod %q not found in stepContext", podName)
		}
		placement, err := sc.cs.MakePlacement(candidateNodes)
		if err != nil {
			t.Fatalf("canSchedule: MakePlacement failed: %v", err)
		}
		_, _, err = sc.cs.CanSchedulePod(sc.ctx, p, placement)
		if err != nil {
			t.Fatalf("CanSchedulePod(%q) unexpected error: %v", podName, err)
		}
	}
}

func verifySnapshot(expected map[string]sets.Set[string]) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		ft.VerifySnapshot(t, sc.snap, expected)
	}
}

func inTransaction(result TransactionResult, steps ...stepFn) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		err := sc.cs.Transaction(sc.ctx, func() (TransactionResult, error) {
			for _, step := range steps {
				step(t, sc)
			}
			return result, nil
		})
		if err != nil {
			t.Fatalf("Transaction failed unexpectedly: %v", err)
		}
	}
}

func inTransactionReturnErr(wantErr string, steps ...stepFn) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		err := sc.cs.Transaction(sc.ctx, func() (TransactionResult, error) {
			for _, step := range steps {
				step(t, sc)
			}
			return Commit, fmt.Errorf("%s", wantErr)
		})
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Transaction expected error containing %q, got: %v", wantErr, err)
		}
	}
}

func expectNestedTxErr() stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		err := sc.cs.Transaction(sc.ctx, func() (TransactionResult, error) {
			return Commit, nil
		})
		wantErr := "a transaction is already in progress"
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("Nested transaction expected error containing %q, got: %v", wantErr, err)
		}
	}
}

func resetMutations() stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		if err := sc.cs.ResetMutations(); err != nil {
			t.Fatalf("ResetMutations failed: %v", err)
		}
	}
}

func resetMutationsErr(wantErr string) stepFn {
	return func(t *testing.T, sc *stepContext) {
		t.Helper()
		err := sc.cs.ResetMutations()
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("ResetMutations expected error containing %q, got: %v", wantErr, err)
		}
	}
}

func TestSnapshot_ActionSequences(t *testing.T) {
	ctx := context.Background()
	nodes := []*v1.Node{
		st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
			v1.ResourceCPU:    "10",
			v1.ResourceMemory: "10Gi",
			v1.ResourcePods:   "110",
		}).Obj(),
		st.MakeNode().Name("singlePodNode").Capacity(map[v1.ResourceName]string{
			v1.ResourcePods: "1",
		}).Obj(),
	}

	pod := st.MakePod().Name("pod").Namespace("default").UID("uid-pod").Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Obj()
	pod3 := st.MakePod().Name("pod3").Namespace("default").UID("uid-pod3").Obj()
	podNoNode := st.MakePod().Name("podNoNode").Namespace("default").UID("uid-nonode").Obj()

	allPods := []*v1.Pod{pod1, pod2, pod3, pod, podNoNode}

	tests := []struct {
		name         string
		assignedPods map[string][]string
		steps        []stepFn
	}{
		{
			name:         "Preempt outside transaction and unpreempt",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
				unpreempt("u1"),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "Unpreempt inside committed transaction in non-LIFO order",
			assignedPods: map[string][]string{"node1": {"pod1", "pod2"}},
			steps: []stepFn{
				inTransaction(Commit,
					preempt("uA", "pod1"),
					preempt("uB", "pod2"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
					unpreempt("uA"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
					unpreempt("uB"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
				),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
			},
		},
		{
			name:         "Mixed preempt and schedule inside reverted transaction rolls back in reverse order",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Revert,
					preempt("u1", "pod1"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
					schedule([]string{"pod"}, []string{"node1"}, SchedulePodsOptions{}),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod")}),
				),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "Non-LIFO unpreemptions inside reverted transaction restore pre-transaction state",
			assignedPods: map[string][]string{"node1": {"pod1", "pod2", "pod3"}},
			steps: []stepFn{
				inTransaction(Revert,
					preempt("u1", "pod1"),
					preempt("u2", "pod2"),
					preempt("u3", "pod3"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
					// Out-of-order unpreemptions
					unpreempt("u2"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod2")}),
					unpreempt("u1"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
				),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2", "pod3")}),
			},
		},
		{
			name:         "PreemptPods fails midway when pod has no node name, restoring prior pods in same call",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preemptErr("has no node name", "pod1", "podNoNode"),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "Handle invalidated by committed transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				inTransaction(Commit, schedule([]string{"pod"}, []string{"node1"}, SchedulePodsOptions{})),
				unpreemptErr("u1", "preemption handle is invalid: snapshot has been permanently mutated since preemption"),
			},
		},
		{
			name:         "Handle not invalidated by reverted transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				inTransaction(Revert, schedule([]string{"pod"}, []string{"node1"}, SchedulePodsOptions{})),
				unpreempt("u1"),
			},
		},
		{
			name:         "Handle invalidated by permanent SchedulePods mutation",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				schedule([]string{"pod"}, []string{"node1"}, SchedulePodsOptions{}),
				unpreemptErr("u1", "preemption handle is invalid: snapshot has been permanently mutated since preemption"),
			},
		},
		{
			name:         "Handle not invalidated by dry-run SchedulePods or read-only CanSchedulePod",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				canSchedule("pod", []string{"node1"}),
				schedule([]string{"pod"}, []string{"node1"}, NewSchedulePodsOptions(true, false)),
				unpreempt("u1"),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "Dry-run rescheduling between preempt and unpreempt restores the original node",
			assignedPods: map[string][]string{"node1": {"pod1"}, "singlePodNode": {}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				schedule([]string{"pod1"}, []string{"singlePodNode"}, NewSchedulePodsOptions(true, false)),
				unpreempt("u1"),
				verifySnapshot(map[string]sets.Set[string]{
					"node1":         sets.New("pod1"),
					"singlePodNode": sets.New[string]()}),
			},
		},
		{
			name:         "Preemption handle created in transaction fails to unpreempt in another transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Commit, preempt("u1", "pod1")),
				inTransaction(Commit, unpreemptErr("u1", "preemption handle is invalid")),
			},
		},
		{
			name:         "Preemption handle created in committed transaction fails to unpreempt outside of transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Commit, preempt("u1", "pod1")),
				unpreemptErr("u1", "preemption handle is invalid"),
			},
		},
		{
			name:         "Preemption handle created in reverted transaction fails to unpreempt outside of transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Revert, preempt("u1", "pod1")),
				unpreemptErr("u1", "preemption handle is invalid"),
			},
		},
		{
			name:         "Preemption handle created outside of transaction fails to unpreempt in transaction",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				inTransaction(Commit, unpreemptErr("u1", "preemption handle is invalid")),
			},
		},
		{
			name:         "Calling Unpreempt second time on the same handle returns error",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				unpreempt("u1"),
				unpreemptErr("u1", "already unpreempted"),
			},
		},
		{
			name:         "Unpreempt nil handle returns error",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				unpreemptErr("nil", "preemption handle is nil"),
			},
		},
		{
			name:         "Transaction returning error automatically rolls back changes",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransactionReturnErr("simulated transaction error",
					preempt("u1", "pod1"),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
				),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "Nested transaction returns error",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Commit,
					expectNestedTxErr(),
				),
			},
		},
		{
			name: "SchedulePods observes mutations",
			assignedPods: map[string][]string{
				"singlePodNode": {},
				"node1":         {}},
			steps: []stepFn{
				inTransaction(Revert,
					schedule([]string{"pod"}, []string{"singlePodNode"}, SchedulePodsOptions{}),
					schedule([]string{"pod1"}, []string{"singlePodNode"}, SchedulePodsOptions{}),
					verifySnapshot(map[string]sets.Set[string]{
						"singlePodNode": sets.New("pod"),
						"node1":         sets.New[string]()}),
					schedule([]string{"pod1"}, []string{"node1"}, SchedulePodsOptions{}),
					verifySnapshot(map[string]sets.Set[string]{
						"singlePodNode": sets.New("pod"),
						"node1":         sets.New("pod1")}),
				),
				schedule([]string{"pod1"}, []string{"singlePodNode"}, SchedulePodsOptions{}),
				verifySnapshot(map[string]sets.Set[string]{
					"singlePodNode": sets.New("pod1"),
					"node1":         sets.New[string]()}),
			},
		},
		{
			name:         "ResetMutations reverts scheduled pods and preemptions",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				schedule([]string{"pod2"}, []string{"node1"}, SchedulePodsOptions{}),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod2")}),
				resetMutations(),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "ResetMutations invalidates preemption handles",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				preempt("u1", "pod1"),
				resetMutations(),
				unpreemptErr("u1", "preemption handle is invalid: snapshot has been permanently mutated since preemption"),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "ResetMutations on empty mutations is no-op",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				resetMutations(),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1")}),
			},
		},
		{
			name:         "ResetMutations inside transaction returns error",
			assignedPods: map[string][]string{"node1": {"pod1"}},
			steps: []stepFn{
				inTransaction(Commit,
					resetMutationsErr("transaction is in progress, cannot reset mutations"),
				),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			podMap := make(map[string]*v1.Pod)
			for _, p := range allPods {
				podMap[p.Name] = p.DeepCopy()
			}
			nodeMap := make(map[string]*v1.Node)
			for _, n := range nodes {
				nodeMap[n.Name] = n
			}
			var assignedPods []*v1.Pod
			var nodesForTest []*v1.Node
			for nodeName, podNames := range tc.assignedPods {
				for _, podName := range podNames {
					p, ok := podMap[podName]
					if !ok {
						t.Fatalf("assigned pod %q not found in predefined pods", podName)
					}
					p.Spec.NodeName = nodeName
					assignedPods = append(assignedPods, p)
				}
				nodesForTest = append(nodesForTest, nodeMap[nodeName])
			}

			cs, snap, _ := setupSnapshotTest(ctx, t, nodesForTest, assignedPods)
			sc := &stepContext{
				ctx:     ctx,
				cs:      cs,
				snap:    snap,
				pods:    podMap,
				handles: make(map[string]*Unpreemption),
			}
			for _, step := range tc.steps {
				step(t, sc)
			}
		})
	}
}

func TestMakePlacement(t *testing.T) {
	node1 := st.MakeNode().Name("node1").Obj()
	node2 := st.MakeNode().Name("node2").Obj()

	cs, _, _ := setupSnapshotTest(context.Background(), t, []*v1.Node{node1, node2}, nil)

	placement, err := cs.MakePlacement([]string{"node1", "node2"})
	if err != nil {
		t.Fatalf("unexpected error from MakePlacement: %v", err)
	}
	if placement == nil || len(placement.Nodes) != 2 {
		t.Fatalf("expected placement with 2 nodes, got %v", placement)
	}

	_, err = cs.MakePlacement([]string{"node1", "non-existent-node"})
	if err == nil {
		t.Fatalf("expected error when node not found in snapshot, got nil")
	}
}

func TestCanSchedulePod(t *testing.T) {
	tests := []struct {
		name           string
		candidateNodes []string
		schedulerName  string
		podRequestCPU  string
		expectNodes    []string
		expectErr      bool
		expectRejected map[string]string
	}{
		{
			name:           "Success - all nodes eligible",
			candidateNodes: []string{"node1", "node2"},
			expectNodes:    []string{"node1", "node2"},
			expectErr:      false,
		},
		{
			name:           "Error - unknown scheduler name",
			candidateNodes: []string{"node1"},
			schedulerName:  "unknown-scheduler",
			expectErr:      true,
		},
		{
			name:           "Success - empty candidate list returns empty result",
			candidateNodes: []string{},
			expectNodes:    nil,
			expectErr:      false,
		},
		{
			name:           "Rejected - insufficient cpu",
			candidateNodes: []string{"node1"},
			podRequestCPU:  "1",
			expectNodes:    []string{},
			expectErr:      false,
			expectRejected: map[string]string{
				"node1": "Insufficient cpu",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			snapshotNodes := make([]*v1.Node, len(tc.candidateNodes))
			for i, name := range tc.candidateNodes {
				snapshotNodes[i] = st.MakeNode().Name(name).Capacity(map[v1.ResourceName]string{
					v1.ResourceCPU:    "0",
					v1.ResourceMemory: "0",
					v1.ResourcePods:   "110",
				}).Obj()
			}

			cs, _, _ := setupSnapshotTest(ctx, t, snapshotNodes, nil)

			podBuilder := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").SchedulerName(tc.schedulerName)
			if tc.podRequestCPU != "" {
				podBuilder = podBuilder.Req(map[v1.ResourceName]string{
					v1.ResourceCPU: tc.podRequestCPU,
				})
			}
			pod := podBuilder.Obj()

			placement, err := cs.MakePlacement(tc.candidateNodes)
			if err != nil && !tc.expectErr {
				t.Fatalf("MakePlacement() error = %v", err)
			}

			nodes, diagnosis, err := cs.CanSchedulePod(ctx, pod, placement)
			if (err != nil) != tc.expectErr {
				t.Fatalf("CanSchedulePod() error = %v, expectErr %v", err, tc.expectErr)
			}

			if !tc.expectErr {
				if diff := cmp.Diff(tc.expectNodes, nodes, cmpopts.EquateEmpty(), cmpopts.SortSlices(func(x, y string) bool { return x < y })); diff != "" {
					t.Errorf("Unexpected nodes (-want +got):\n%s", diff)
				}

				if len(tc.expectRejected) > 0 {
					if diagnosis == nil {
						t.Errorf("Expected diagnosis, got nil")
					} else {
						for nodeName, expectedMsg := range tc.expectRejected {
							status := diagnosis.NodeToStatus.Get(nodeName)
							if status == nil {
								t.Errorf("Expected status for node %s, got nil", nodeName)
							} else if !strings.Contains(status.Message(), expectedMsg) {
								t.Errorf("Expected status message %q to contain %q for node %s", status.Message(), expectedMsg, nodeName)
							}
						}
					}
				}
			}
		})
	}
}

var scheduleResultCmpOpts = []cmp.Option{
	cmpopts.EquateEmpty(),
	cmp.Comparer(func(x, y *fwk.Status) bool {
		return x.Code() == y.Code()
	}),
	cmpopts.IgnoreFields(SchedulingResult{}, "CycleState"),
}

// podNameCmpOpt compares pods generated from a template by namespace and name, ignoring the random
// UID that createPodFromTemplate appends to the name. The suffix is stripped from both sides,
// as go-cmp requires the comparer to be symmetric; only the generated pod actually carries one.
var podNameCmpOpt = cmp.Comparer(func(x, y *v1.Pod) bool {
	if x == nil || y == nil {
		return x == y
	}
	return x.Namespace == y.Namespace && trimGeneratedUID(x) == trimGeneratedUID(y)
})

// trimGeneratedUID drops the trailing "-<uid>" that createPodFromTemplate appends to the pod name,
// leaving the deterministic "<template-name>-<index>" part. Pods carrying no UID - the expected
// ones - keep their name as is.
func trimGeneratedUID(p *v1.Pod) string {
	if p.UID == "" {
		return p.Name
	}
	return strings.TrimSuffix(p.Name, "-"+string(p.UID))
}

func TestSchedulePods(t *testing.T) {
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "2"}).Obj()
	node1Capacity1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "1"}).Obj()
	node1Unschedulable := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "0"}).Obj()
	node2Unschedulable := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "0"}).Obj()

	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()
	pod1WithErr := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").SchedulerName("non-existent-scheduler").Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Obj()
	pod2WithErr := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").SchedulerName("non-existent-scheduler").Obj()
	pod3 := st.MakePod().Name("pod3").Namespace("default").UID("uid-pod3").Obj()
	pod4 := st.MakePod().Name("pod4").Namespace("default").UID("uid-pod4").Obj()

	// onNode is how a scheduled pod comes back: a copy carrying the node the attempt selected.
	onNode := func(p *v1.Pod, nodeName string) *v1.Pod {
		p = p.DeepCopy()
		p.Spec.NodeName = nodeName
		return p
	}

	tests := []struct {
		name                string
		nodes               []*v1.Node
		pods                []*v1.Pod
		candidateNodes      []string
		opts                SchedulePodsOptions
		expectResults       []SchedulingResult
		expectSnapshotState map[string]sets.Set[string]
		expectErr           bool
	}{
		{
			name:           "Success - schedule one pod",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1},
			candidateNodes: []string{"node1"},
			opts:           SchedulePodsOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name:           "DryRun - does not persist",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(true, false),
			expectResults: []SchedulingResult{
				{
					// The dry run is reported like any other attempt; only the snapshot is restored.
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:                "StopOnFailure - fails on first pod",
			nodes:               []*v1.Node{node1},
			pods:                []*v1.Pod{pod1WithErr},
			candidateNodes:      []string{"node1"},
			opts:                NewSchedulePodsOptions(false, true),
			expectResults:       nil,
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
			expectErr:           true,
		},
		{
			name:           "Fails due to node unschedulable",
			nodes:          []*v1.Node{node1Unschedulable},
			pods:           []*v1.Pod{pod1},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(false, true),
			expectResults: []SchedulingResult{
				{
					Pod:              pod1,
					SelectedNodeName: "",
					Status:           fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:           "Schedule over capacity without stopping on failure",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1, pod2, pod3, pod4},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(false, false),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:              onNode(pod2, "node1"),
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:    pod3,
					Status: fwk.NewStatus(fwk.Unschedulable),
				},
				{
					Pod:    pod4,
					Status: fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("pod1", "pod2"),
			},
		},
		{
			name:           "Schedule over capacity with stopping on failure",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1, pod2, pod3, pod4},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(false, true),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:              onNode(pod2, "node1"),
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:    pod3,
					Status: fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("pod1", "pod2"),
			},
		},
		{
			name:           "StopOnFailure - succeeds on first, fails on second due to node unschedulable",
			nodes:          []*v1.Node{node1Capacity1, node2Unschedulable},
			pods:           []*v1.Pod{pod1, pod2},
			candidateNodes: []string{"node1", "node2"},
			opts:           NewSchedulePodsOptions(false, true),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              pod2,
					SelectedNodeName: "",
					Status:           fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": nil},
		},
		{
			name:           "StopOnFailure - stops on first failure even if more pods could be scheduled",
			nodes:          []*v1.Node{node1Capacity1, node2Unschedulable},
			pods:           []*v1.Pod{pod1, pod2, pod3},
			candidateNodes: []string{"node1", "node2"},
			opts:           NewSchedulePodsOptions(false, true),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              pod2,
					SelectedNodeName: "",
					Status:           fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": nil},
		},
		{
			name:           "Error outside transaction - rolls back previous successful pods",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1, pod2WithErr},
			candidateNodes: []string{"node1"},
			opts:           SchedulePodsOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
			expectErr:           true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			cs, snap, _ := setupSnapshotTest(ctx, t, tc.nodes, nil)

			placement, err := cs.MakePlacement(tc.candidateNodes)
			if err != nil && !tc.expectErr {
				t.Fatalf("MakePlacement() error = %v, expectErr %v", err, tc.expectErr)
			}

			// The fixtures are shared by the cases, so each one works on its own copies.
			pods := make([]*v1.Pod, 0, len(tc.pods))
			for _, p := range tc.pods {
				pods = append(pods, p.DeepCopy())
			}

			results, err := cs.SchedulePods(ctx, pods, placement, tc.opts)
			if (err != nil) != tc.expectErr {
				t.Fatalf("SchedulePods() error = %v, expectErr %v", err, tc.expectErr)
			}

			if diff := cmp.Diff(tc.expectResults, results, scheduleResultCmpOpts...); diff != "" {
				t.Errorf("Unexpected scheduling results (-want +got):\n%s", diff)
			}

			// The pods belong to the caller, so the simulation must not write to them.
			if diff := cmp.Diff(tc.pods, pods); diff != "" {
				t.Errorf("SchedulePods mutated the pods it was given (-before +after):\n%s", diff)
			}

			// Successful results report the selected node on the Pod they return.
			for _, res := range results {
				if !res.Status.IsSuccess() {
					continue
				}
				if res.Pod.Spec.NodeName != res.SelectedNodeName {
					t.Errorf("expected pod %s to have NodeName %q, got %q", res.Pod.Name, res.SelectedNodeName, res.Pod.Spec.NodeName)
				}
				if res.CycleState == nil {
					t.Errorf("expected CycleState to be populated for scheduled pod %s", res.Pod.Name)
				}
			}

			ft.VerifySnapshot(t, snap, tc.expectSnapshotState)
		})
	}
}

// The pod handed to SchedulePods is not the one the snapshot ends up holding: the result carries
// the copy that was placed, and that copy is what PreemptPods takes to remove it again.
func TestSchedulePodsResultFeedsPreemptPods(t *testing.T) {
	ctx := context.Background()
	node := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "1"}).Obj()
	pod := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Obj()

	cs, snap, _ := setupSnapshotTest(ctx, t, []*v1.Node{node}, nil)
	placement, err := cs.MakePlacement([]string{"node1"})
	if err != nil {
		t.Fatalf("MakePlacement() error = %v", err)
	}

	results, err := cs.SchedulePods(ctx, []*v1.Pod{pod}, placement, SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods() error = %v", err)
	}
	ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("pod1")})

	if _, err := cs.PreemptPods(ctx, []*v1.Pod{results[0].Pod}); err != nil {
		t.Fatalf("PreemptPods(results[0].Pod) error = %v", err)
	}
	ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New[string]()})
}

func TestSchedulePodsByTemplate(t *testing.T) {
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "3"}).Obj()

	defaultTemplate := &v1.PodTemplateSpec{Spec: v1.PodSpec{}}
	customNSTemplate := &v1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Namespace: "custom-ns"},
		Spec:       v1.PodSpec{},
	}

	// createPodFromTemplate names the pods it generates "<template-name>-<index>-<uuid>", and the
	// UUID is random. The expected pods below therefore carry only the deterministic part of the
	// name, and podNameCmpOpt strips the UUID suffix from the generated one before comparing.
	// Keeping the index in the expectation makes the comparison exact.
	generatedPod := func(index int) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("templated-pod-%d", index), Namespace: "default"}}
	}
	generatedNSPod := func(index int) *v1.Pod {
		return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("templated-pod-%d", index), Namespace: "custom-ns"}}
	}

	tests := []struct {
		name                string
		template            *v1.PodTemplateSpec
		nodes               []*v1.Node
		candidateNodes      []string
		maxPods             int
		opts                SchedulePodsByTemplateOptions
		expectResults       []SchedulingResult
		expectSnapshotState map[string]int
		expectErr           bool
	}{
		{
			name:           "Success - schedule maxPods",
			template:       defaultTemplate,
			nodes:          []*v1.Node{node1},
			candidateNodes: []string{"node1"},
			maxPods:        2,
			opts:           SchedulePodsByTemplateOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              generatedPod(0),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              generatedPod(1),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]int{"node1": 2},
			expectErr:           false,
		},
		{
			name:           "Max pods over candidate capacity",
			template:       defaultTemplate,
			nodes:          []*v1.Node{node1},
			candidateNodes: []string{"node1"},
			maxPods:        5,
			opts:           SchedulePodsByTemplateOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              generatedPod(0),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              generatedPod(1),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              generatedPod(2),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:    generatedPod(3),
					Status: fwk.NewStatus(fwk.Unschedulable),
				},
			},
			expectSnapshotState: map[string]int{"node1": 3},
			expectErr:           false,
		},
		{
			name:                "Zero maxPods returns nil",
			template:            defaultTemplate,
			nodes:               []*v1.Node{node1},
			candidateNodes:      []string{"node1"},
			maxPods:             0,
			opts:                SchedulePodsByTemplateOptions{},
			expectResults:       nil,
			expectSnapshotState: map[string]int{"node1": 0},
			expectErr:           false,
		},
		{
			name:                "Empty candidateNodes returns nil",
			template:            defaultTemplate,
			nodes:               nil,
			candidateNodes:      []string{},
			maxPods:             2,
			opts:                SchedulePodsByTemplateOptions{},
			expectResults:       nil,
			expectSnapshotState: map[string]int{},
			expectErr:           false,
		},
		{
			name:           "DryRun - does not persist",
			template:       defaultTemplate,
			nodes:          []*v1.Node{node1},
			candidateNodes: []string{"node1"},
			maxPods:        2,
			opts:           NewSchedulePodsByTemplateOptions(true),
			expectResults: []SchedulingResult{
				{
					Pod:              generatedPod(0),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:              generatedPod(1),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]int{"node1": 0},
			expectErr:           false,
		},
		{
			name:           "Custom Namespace",
			template:       customNSTemplate,
			nodes:          []*v1.Node{node1},
			candidateNodes: []string{"node1"},
			maxPods:        1,
			opts:           SchedulePodsByTemplateOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              generatedNSPod(0),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]int{"node1": 1},
			expectErr:           false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			cs, snap, _ := setupSnapshotTest(ctx, t, tc.nodes, nil)

			placement, err := cs.MakePlacement(tc.candidateNodes)
			if err != nil && !tc.expectErr {
				t.Fatalf("MakePlacement() error = %v, expectErr %v", err, tc.expectErr)
			}

			results, err := cs.SchedulePodsByTemplate(ctx, tc.template, placement, tc.maxPods, tc.opts)
			if (err != nil) != tc.expectErr {
				t.Fatalf("SchedulePodsByTemplate() error = %v, expectErr %v", err, tc.expectErr)
			}

			var opts []cmp.Option
			opts = append(opts, scheduleResultCmpOpts...)
			opts = append(opts, podNameCmpOpt)

			if diff := cmp.Diff(tc.expectResults, results, opts...); diff != "" {
				t.Errorf("Unexpected scheduling results (-want +got):\n%s", diff)
			}

			ft.VerifySnapshotPodCounts(t, snap, tc.expectSnapshotState)
		})
	}
}

func TestResetMutations_NodeGenerationRestored(t *testing.T) {
	ctx := context.Background()

	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "10",
		v1.ResourceMemory: "10Gi",
		v1.ResourcePods:   "110",
	}).Obj()

	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	pod2 := st.MakePod().Name("pod2").Namespace("default").UID("uid-pod2").Node("node2").Obj()
	pod3 := st.MakePod().Name("pod3").Namespace("default").UID("uid-pod3").Obj()

	cs, snap, _ := setupSnapshotTest(ctx, t, []*v1.Node{node1, node2}, []*v1.Pod{pod1, pod2})

	getGenerations := func() map[string]int64 {
		t.Helper()
		generations := make(map[string]int64)
		nodes, err := snap.NodeInfos().List()
		if err != nil {
			t.Fatalf("NodeInfos().List() failed: %v", err)
		}
		for _, node := range nodes {
			generations[node.Node().Name] = node.GetGeneration()
		}
		return generations
	}

	// Record initial generations for all nodes
	initialGenerations := getGenerations()

	// Update node1 by removing pod1 from it
	_, err := cs.PreemptPods(ctx, []*v1.Pod{pod1})
	if err != nil {
		t.Fatalf("PreemptPods failed: %v", err)
	}

	placement, err := cs.MakePlacement([]string{"node2"})
	if err != nil {
		t.Fatalf("MakePlacement failed: %v", err)
	}

	// Update node2 by scheduling pod3 to it
	_, err = cs.SchedulePods(ctx, []*v1.Pod{pod3}, placement, SchedulePodsOptions{})
	if err != nil {
		t.Fatalf("SchedulePods failed: %v", err)
	}

	if err := cs.ResetMutations(); err != nil {
		t.Fatalf("ResetMutations failed: %v", err)
	}

	// Compare generations after reset with initial generations
	if diff := cmp.Diff(initialGenerations, getGenerations()); diff != "" {
		t.Errorf("Generations don't match (-want +got):\n%s", diff)
	}
}

func TestScheduleWorkload(t *testing.T) {
	ctx := context.Background()

	node4CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPG := testutils.MakeGangPodGroup("gang-pg", "", 2)
	disjointPG := testutils.MakeGangPodGroup("disjoint-pg", "", 1)

	rootCPG := testutils.MakeGangCompositePodGroup("root-cpg", "", 2)
	leafPG1 := testutils.MakeGangPodGroup("leaf-1", "root-cpg", 1)
	leafPG2 := testutils.MakeGangPodGroup("leaf-2", "root-cpg", 1)

	pod1 := testutils.MakePod("pod1", "gang-pg", "2")
	pod2 := testutils.MakePod("pod2", "gang-pg", "2")

	heavyPod1 := testutils.MakePod("hp1", "gang-pg", "3")
	heavyPod2 := testutils.MakePod("hp2", "gang-pg", "3")

	leafPod1 := testutils.MakePod("lp1", "leaf-1", "2")
	leafPod2 := testutils.MakePod("lp2", "leaf-2", "2")

	disjointPod := testutils.MakePod("dp", "disjoint-pg", "1")
	noPGPod := testutils.MakePod("no-pg", "", "")
	missingPGPod := testutils.MakePod("missing-pg", "missing-pg-name", "")

	tests := []struct {
		name                string
		nodes               []*v1.Node
		podGroups           []*schedulingv1beta1.PodGroup
		compositePodGroups  []*schedulingv1alpha3.CompositePodGroup
		pods                []*v1.Pod
		opts                ScheduleWorkloadOptions
		expectResults       []SchedulingResult
		expectSnapshotState map[string]sets.Set[string]
		expectErr           bool
	}{
		{
			name:               "Success - schedule flat pod group",
			nodes:              []*v1.Node{node8CPU},
			podGroups:          []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups: nil,
			pods:               []*v1.Pod{pod1, pod2},
			opts:               NewScheduleWorkloadOptions(false),
			expectResults: []SchedulingResult{
				{Pod: pod1, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
				{Pod: pod2, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")},
		},
		{
			name:               "Success - schedule composite pod group",
			nodes:              []*v1.Node{node8CPU},
			podGroups:          []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG},
			pods:               []*v1.Pod{leafPod1, leafPod2},
			opts:               NewScheduleWorkloadOptions(false),
			expectResults: []SchedulingResult{
				{Pod: leafPod1, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
				{Pod: leafPod2, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("lp1", "lp2")},
		},
		{
			name:               "DryRun - returns results without persisting to snapshot",
			nodes:              []*v1.Node{node8CPU},
			podGroups:          []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups: nil,
			pods:               []*v1.Pod{pod1, pod2},
			opts:               NewScheduleWorkloadOptions(true),
			expectResults: []SchedulingResult{
				{Pod: pod1, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
				{Pod: pod2, SelectedNodeName: "node1", Status: fwk.NewStatus(fwk.Success)},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:               "Failure - gang unschedulable rolls back snapshot reservations",
			nodes:              []*v1.Node{node4CPU},
			podGroups:          []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups: nil,
			pods:               []*v1.Pod{heavyPod1, heavyPod2},
			opts:               NewScheduleWorkloadOptions(false),
			expectResults: []SchedulingResult{
				{Pod: heavyPod1, SelectedNodeName: "", Status: fwk.NewStatus(fwk.Unschedulable, "no pods were schedulable")},
				{Pod: heavyPod2, SelectedNodeName: "", Status: fwk.NewStatus(fwk.Unschedulable, "pod group is unschedulable")},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:                "Empty pod list returns nil",
			nodes:               []*v1.Node{node8CPU},
			podGroups:           []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups:  nil,
			pods:                nil,
			opts:                NewScheduleWorkloadOptions(false),
			expectResults:       nil,
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:                "Validation error - pod not member of any PodGroup",
			nodes:               []*v1.Node{node8CPU},
			podGroups:           []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups:  nil,
			pods:                []*v1.Pod{noPGPod},
			opts:                NewScheduleWorkloadOptions(false),
			expectResults:       nil,
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
			expectErr:           true,
		},
		{
			name:                "Validation error - pod group not found in snapshot",
			nodes:               []*v1.Node{node8CPU},
			podGroups:           []*schedulingv1beta1.PodGroup{gangPG},
			compositePodGroups:  nil,
			pods:                []*v1.Pod{missingPGPod},
			opts:                NewScheduleWorkloadOptions(false),
			expectResults:       nil,
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
			expectErr:           true,
		},
		{
			name:                "Validation error - pods belong to disjoint hierarchies",
			nodes:               []*v1.Node{node8CPU},
			podGroups:           []*schedulingv1beta1.PodGroup{gangPG, disjointPG},
			compositePodGroups:  nil,
			pods:                []*v1.Pod{pod1, disjointPod},
			opts:                NewScheduleWorkloadOptions(false),
			expectResults:       nil,
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
			expectErr:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				nil,
				tt.nodes,
				tt.podGroups,
				tt.compositePodGroups,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			cs := New(snap, profileMap)

			result := cs.ScheduleWorkload(ctx, tt.pods, tt.opts)
			if result.Status.IsError() != tt.expectErr {
				t.Fatalf("ScheduleWorkload() error = %v, expectErr %v", result.Status.AsError(), tt.expectErr)
			}

			if !tt.expectErr && len(tt.expectResults) > 0 {
				if len(result.PodResults) != len(tt.expectResults) {
					t.Fatalf("ScheduleWorkload() got %d results, want %d", len(result.PodResults), len(tt.expectResults))
				}
				for i := range result.PodResults {
					if result.PodResults[i].SelectedNodeName != tt.expectResults[i].SelectedNodeName {
						t.Errorf("result[%d] SelectedNodeName = %q, want %q", i, result.PodResults[i].SelectedNodeName, tt.expectResults[i].SelectedNodeName)
					}
					if result.PodResults[i].Status.IsSuccess() != tt.expectResults[i].Status.IsSuccess() {
						t.Errorf("result[%d] Status.IsSuccess = %v, want %v", i, result.PodResults[i].Status.IsSuccess(), tt.expectResults[i].Status.IsSuccess())
					}
				}
			}

			ft.VerifySnapshot(t, snap, tt.expectSnapshotState)
		})
	}
}

func TestSnapshot_ActionSequences_ScheduleWorkload(t *testing.T) {
	ctx := context.Background()

	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	pg1 := testutils.MakeGangPodGroup("gang-pg1", "", 2)
	pg2 := testutils.MakeGangPodGroup("gang-pg2", "", 2)

	pod1 := testutils.MakePod("pod1", "gang-pg1", "2")
	pod2 := testutils.MakePod("pod2", "gang-pg1", "2")
	pod3 := testutils.MakePod("pod3", "gang-pg2", "2")
	pod4 := testutils.MakePod("pod4", "gang-pg2", "2")

	extraPod := testutils.MakePod("extra-pod", "", "1")

	allPods := []*v1.Pod{pod1, pod2, pod3, pod4, extraPod}

	tests := []struct {
		name  string
		steps []stepFn
	}{
		{
			name: "ScheduleWorkload observes mutations",
			steps: []stepFn{
				schedule([]string{"extra-pod"}, []string{"node1"}, SchedulePodsOptions{}),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("extra-pod")}),
				scheduleWorkload([]string{"pod1", "pod2"}, NewScheduleWorkloadOptions(false) /* expect failure */, false),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("extra-pod")}),
				resetMutations(),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
				scheduleWorkload([]string{"pod1", "pod2"}, NewScheduleWorkloadOptions(false) /* expect success */, true),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
			},
		},
		{
			name: "Transactional ScheduleWorkload reservations are undone on revert",
			steps: []stepFn{
				inTransaction(Revert,
					scheduleWorkload([]string{"pod1", "pod2"}, NewScheduleWorkloadOptions(false) /* expect success */, true),
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
				),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()}),
			},
		},
		{
			name: "ScheduleWorkload reservations on snapshot are persisted across calls",
			steps: []stepFn{
				scheduleWorkload([]string{"pod1", "pod2"}, NewScheduleWorkloadOptions(false) /* expect success */, true),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
				scheduleWorkload([]string{"pod3", "pod4"}, NewScheduleWorkloadOptions(false) /* expect failure */, false),
				verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")}),
			},
		},
		{
			name: "AddPodGroups and RemovePodGroups integrate with Transaction, ResetMutations, and ScheduleWorkload",
			steps: []stepFn{
				func(t *testing.T, sc *stepContext) {
					pg3 := testutils.MakeGangPodGroup("gang-pg3", "", 1)
					pod5 := testutils.MakePod("pod5", "gang-pg3", "1")
					sc.pods["pod5"] = pod5

					// Removing pg1 inside a reverted transaction makes pg1 workload fail, then restores pg1 on revert.
					inTransaction(Revert,
						func(t *testing.T, sc *stepContext) {
							if err := sc.cs.RemovePodGroup(ctx, pg1); err != nil {
								t.Fatalf("RemovePodGroup failed: %v", err)
							}
							verifySnapshotPodGroups(t, sc.snap, []*schedulingv1beta1.PodGroup{pg2}, nil)
						},
						scheduleWorkload([]string{"pod1", "pod2"}, NewScheduleWorkloadOptions(false), false),
					)(t, sc)
					verifySnapshotPodGroups(t, sc.snap, []*schedulingv1beta1.PodGroup{pg1, pg2}, nil)

					// Adding pg3 allows scheduling pod5, and ResetMutations reverts both pod5 and pg3.
					if err := sc.cs.AddPodGroup(ctx, pg3); err != nil {
						t.Fatalf("AddPodGroup failed: %v", err)
					}
					verifySnapshotPodGroups(t, sc.snap, []*schedulingv1beta1.PodGroup{pg1, pg2, pg3}, nil)
					scheduleWorkload([]string{"pod5"}, NewScheduleWorkloadOptions(false), true)(t, sc)
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New("pod5")})(t, sc)

					resetMutations()(t, sc)
					verifySnapshot(map[string]sets.Set[string]{"node1": sets.New[string]()})(t, sc)
					verifySnapshotPodGroups(t, sc.snap, []*schedulingv1beta1.PodGroup{pg1, pg2}, nil)
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			podMap := make(map[string]*v1.Pod)
			for _, p := range allPods {
				podMap[p.Name] = p.DeepCopy()
			}

			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				nil,
				[]*v1.Node{node1},
				[]*schedulingv1beta1.PodGroup{pg1, pg2},
				nil,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			cs := New(snap, profileMap)
			sc := &stepContext{
				ctx:     ctx,
				cs:      cs,
				snap:    snap,
				pods:    podMap,
				handles: make(map[string]*Unpreemption),
			}
			for _, step := range tc.steps {
				step(t, sc)
			}
		})
	}
}

// UPSTREAM-DIFF: Upstream kubernetes PR #142177 (https://github.com/kubernetes/kubernetes/pull/142177)
// provides TestSnapshot_AddGenericPodGroup and TestSnapshot_RemoveGenericPodGroup in k8s.io/kubernetes.
//
// Test Migration Plan:
// When go.mod updates k8s.io/kubernetes to include PR #142177:
// 1. Delete verifySnapshotPodGroups below. Upstream tests already verify the internal map state of cache.Snapshot.
// 2. Simplify TestSnapshot_AddPodGroups and TestSnapshot_RemovePodGroups. Upstream tests already cover tree permutations.
// 3. Keep tests focused on ClusterSnapshot responsibilities: transaction rollback (undoLog) and preemption version bumps.

// verifySnapshotPodGroups verifies the internal pod group and composite pod group state of cache.Snapshot.
// It uses reflection to inspect unexported snapshot maps until upstream cache.Snapshot exposes inspection methods.
func verifySnapshotPodGroups(
	t *testing.T,
	snap *cache.Snapshot,
	wantPodGroups []*schedulingv1beta1.PodGroup,
	wantCompositePodGroups []*schedulingv1alpha3.CompositePodGroup,
) {
	t.Helper()

	rv := reflect.ValueOf(snap).Elem()
	podGroupStates := writableField(rv, "podGroupStates")
	compositePodGroupStates := writableField(rv, "compositePodGroupStates")

	wantPGMap := make(map[fwk.EntityKey]*schedulingv1beta1.PodGroup, len(wantPodGroups))
	wantChildren := make(map[fwk.EntityKey]sets.Set[fwk.EntityKey])
	for _, pg := range wantPodGroups {
		key := fwk.PodGroupKey(pg.Namespace, pg.Name)
		wantPGMap[key] = pg
		gotPG, err := snap.PodGroups().Get(pg.Namespace, pg.Name)
		if err != nil {
			t.Errorf("snap.PodGroups().Get(%s, %s) unexpected error: %v", pg.Namespace, pg.Name, err)
		} else if diff := cmp.Diff(pg, gotPG); diff != "" {
			t.Errorf("snap.PodGroups().Get(%s, %s) mismatch (-want +got):\n%s", pg.Namespace, pg.Name, diff)
		}
		if pg.Spec.ParentCompositePodGroupName != nil && *pg.Spec.ParentCompositePodGroupName != "" {
			parentKey := fwk.CompositePodGroupKey(pg.Namespace, *pg.Spec.ParentCompositePodGroupName)
			if wantChildren[parentKey] == nil {
				wantChildren[parentKey] = sets.New[fwk.EntityKey]()
			}
			wantChildren[parentKey].Insert(key)
		}
	}

	gotPGMap := make(map[fwk.EntityKey]*schedulingv1beta1.PodGroup)
	pgIter := podGroupStates.MapRange()
	for pgIter.Next() {
		key := pgIter.Key().Interface().(fwk.EntityKey)
		pgs := pgIter.Value()
		if podGroupEmpty(pgs) {
			t.Errorf("empty podGroupStateSnapshot left in snapshot for key %s", key)
		}
		if pgField := writableField(pgs.Elem(), "podGroup"); !pgField.IsNil() {
			gotPGMap[key] = pgField.Interface().(*schedulingv1beta1.PodGroup)
		}
	}
	if diff := cmp.Diff(wantPGMap, gotPGMap, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("snapshot podGroups mismatch (-want +got):\n%s", diff)
	}

	wantCPGMap := make(map[fwk.EntityKey]*schedulingv1alpha3.CompositePodGroup, len(wantCompositePodGroups))
	for _, cpg := range wantCompositePodGroups {
		key := fwk.CompositePodGroupKey(cpg.Namespace, cpg.Name)
		wantCPGMap[key] = cpg
		if wantChildren[key] == nil {
			wantChildren[key] = sets.New[fwk.EntityKey]()
		}
		gotCPG, err := snap.CompositePodGroups().Get(cpg.Namespace, cpg.Name)
		if err != nil {
			t.Errorf("snap.CompositePodGroups().Get(%s, %s) unexpected error: %v", cpg.Namespace, cpg.Name, err)
		} else if diff := cmp.Diff(cpg, gotCPG); diff != "" {
			t.Errorf("snap.CompositePodGroups().Get(%s, %s) mismatch (-want +got):\n%s", cpg.Namespace, cpg.Name, diff)
		}
		if cpg.Spec.ParentCompositePodGroupName != nil && *cpg.Spec.ParentCompositePodGroupName != "" {
			parentKey := fwk.CompositePodGroupKey(cpg.Namespace, *cpg.Spec.ParentCompositePodGroupName)
			if wantChildren[parentKey] == nil {
				wantChildren[parentKey] = sets.New[fwk.EntityKey]()
			}
			wantChildren[parentKey].Insert(key)
		}
	}

	gotCPGMap := make(map[fwk.EntityKey]*schedulingv1alpha3.CompositePodGroup)
	gotChildren := make(map[fwk.EntityKey]sets.Set[fwk.EntityKey])
	cpgIter := compositePodGroupStates.MapRange()
	for cpgIter.Next() {
		key := cpgIter.Key().Interface().(fwk.EntityKey)
		cpgs := cpgIter.Value()
		if compositePodGroupEmpty(cpgs) {
			t.Errorf("empty compositePodGroupStateSnapshot left in snapshot for key %s", key)
		}
		if cpgField := writableField(cpgs.Elem(), "compositePodGroup"); !cpgField.IsNil() {
			gotCPGMap[key] = cpgField.Interface().(*schedulingv1alpha3.CompositePodGroup)
		}
		state, err := snap.CompositePodGroupStates().Get(key.Namespace, key.Name)
		if err != nil {
			t.Errorf("snap.CompositePodGroupStates().Get(%s, %s) unexpected error: %v", key.Namespace, key.Name, err)
		} else {
			gotChildren[key] = sets.New(state.GetChildren()...)
		}
	}
	if diff := cmp.Diff(wantCPGMap, gotCPGMap, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("snapshot compositePodGroups mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantChildren, gotChildren, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("snapshot compositePodGroup children mismatch (-want +got):\n%s", diff)
	}
}

// TestSnapshot_AddPodGroups tests adding PodGroups and CompositePodGroups to ClusterSnapshot.
// When k8s.io/kubernetes includes PR #142177, this test will verify the calls delegated
// to cache.Snapshot.AddGenericPodGroup.
func TestSnapshot_AddPodGroups(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.TopologyAwareWorkloadScheduling: true,
		features.GenericWorkload:                 true,
		features.CompositePodGroup:               true,
	})

	pg1 := testutils.MakeBasicPodGroup("pg1", "")
	pg2 := testutils.MakeBasicPodGroup("pg2", "")
	rootCPG := testutils.MakeBasicCompositePodGroup("root-cpg", "")
	cpg1 := testutils.MakeBasicCompositePodGroup("cpg1", "root-cpg")
	cpg2 := testutils.MakeBasicCompositePodGroup("cpg2", "root-cpg")
	leafPG1 := testutils.MakeBasicPodGroup("leaf-pg1", "cpg1")
	leafPG2 := testutils.MakeBasicPodGroup("leaf-pg2", "cpg1")
	podInPG1 := testutils.MakePod("pod1", "pg1", "1")

	tests := []struct {
		name                   string
		initPods               []*v1.Pod
		initPodGroups          []*schedulingv1beta1.PodGroup
		initCompositePodGroups []*schedulingv1alpha3.CompositePodGroup
		addPodGroups           []*schedulingv1beta1.PodGroup
		addCompositePodGroups  []*schedulingv1alpha3.CompositePodGroup
		wantPodGroups          []*schedulingv1beta1.PodGroup
		wantCompositePodGroups []*schedulingv1alpha3.CompositePodGroup
		wantErr                bool
	}{
		{
			name:          "Add flat pod groups to empty snapshot",
			addPodGroups:  []*schedulingv1beta1.PodGroup{pg1, pg2},
			wantPodGroups: []*schedulingv1beta1.PodGroup{pg1, pg2},
		},
		{
			name:                   "Add hierarchical pod groups and composite pod groups",
			addPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			addCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
		},
		{
			name:                   "Add child pod group when parent composite pod group already exists",
			initCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			addPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
		},
		{
			name:                   "Add parent composite pod group when child pod group already exists",
			initPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1},
			addCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{cpg1},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{cpg1},
		},
		{
			name:          "Add pod group when pods belonging to it already exist in snapshot",
			initPods:      []*v1.Pod{podInPG1},
			addPodGroups:  []*schedulingv1beta1.PodGroup{pg1},
			wantPodGroups: []*schedulingv1beta1.PodGroup{pg1},
		},
		{
			name:          "Fallback on error when adding already existing pod group",
			initPodGroups: []*schedulingv1beta1.PodGroup{pg2},
			addPodGroups:  []*schedulingv1beta1.PodGroup{pg1, pg2},
			wantPodGroups: []*schedulingv1beta1.PodGroup{pg2},
			wantErr:       true,
		},
		{
			name:          "Fallback on error when adding duplicate pod groups in same call",
			addPodGroups:  []*schedulingv1beta1.PodGroup{leafPG1, leafPG1},
			wantPodGroups: nil,
			wantErr:       true,
		},
		{
			name:                   "Fallback on error when adding already existing composite pod group rolls back added pod groups and composite pod groups",
			initCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{cpg2},
			addPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			addCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			wantPodGroups:          nil,
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{cpg2},
			wantErr:                true,
		},
		{
			name:                   "Fallback on error when adding duplicate composite pod groups in same call rolls back all additions",
			addPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			addCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{cpg1, cpg1},
			wantPodGroups:          nil,
			wantCompositePodGroups: nil,
			wantErr:                true,
		},
		{
			name:          "Fallback on error preserves pre-existing pods in podGroupState",
			initPods:      []*v1.Pod{podInPG1},
			initPodGroups: []*schedulingv1beta1.PodGroup{pg2},
			addPodGroups:  []*schedulingv1beta1.PodGroup{pg1, pg2},
			wantPodGroups: []*schedulingv1beta1.PodGroup{pg2},
			wantErr:       true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := cache.NewTestSnapshotWithCompositePodGroups(tc.initPods, nil, tc.initPodGroups, tc.initCompositePodGroups)
			cs := New(snap, nil)

			err := cs.Transaction(t.Context(), func() (TransactionResult, error) {
				for _, cpg := range tc.addCompositePodGroups {
					if err := cs.AddCompositePodGroup(t.Context(), cpg); err != nil {
						return Revert, err
					}
				}
				for _, pg := range tc.addPodGroups {
					if err := cs.AddPodGroup(t.Context(), pg); err != nil {
						return Revert, err
					}
				}
				return Commit, nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Add operations error = %v, wantErr %v", err, tc.wantErr)
			}

			verifySnapshotPodGroups(t, snap, tc.wantPodGroups, tc.wantCompositePodGroups)

			if len(tc.initPods) > 0 {
				pgState, err := snap.PodGroupStates().Get(podInPG1.Namespace, "pg1")
				if err != nil {
					t.Fatalf("expected pod group state for pg1 to exist, got error: %v", err)
				}
				if pgState.AllPodsCount() != len(tc.initPods) {
					t.Errorf("expected AllPodsCount() = %d, got %d", len(tc.initPods), pgState.AllPodsCount())
				}
			}
		})
	}
}

// TestSnapshot_RemovePodGroups tests removing PodGroups and CompositePodGroups from ClusterSnapshot.
// When k8s.io/kubernetes includes PR #142177, this test will verify the calls delegated
// to cache.Snapshot.RemoveGenericPodGroup.
func TestSnapshot_RemovePodGroups(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.TopologyAwareWorkloadScheduling: true,
		features.GenericWorkload:                 true,
		features.CompositePodGroup:               true,
	})

	pg1 := testutils.MakeBasicPodGroup("pg1", "")
	pg2 := testutils.MakeBasicPodGroup("pg2", "")
	rootCPG := testutils.MakeBasicCompositePodGroup("root-cpg", "")
	cpg1 := testutils.MakeBasicCompositePodGroup("cpg1", "root-cpg")
	cpg2 := testutils.MakeBasicCompositePodGroup("cpg2", "root-cpg")
	leafPG1 := testutils.MakeBasicPodGroup("leaf-pg1", "cpg1")
	leafPG2 := testutils.MakeBasicPodGroup("leaf-pg2", "cpg1")
	podInPG1 := testutils.MakePod("pod1", "pg1", "1")

	tests := []struct {
		name                    string
		initPods                []*v1.Pod
		initPodGroups           []*schedulingv1beta1.PodGroup
		initCompositePodGroups  []*schedulingv1alpha3.CompositePodGroup
		removePodGroups         []*schedulingv1beta1.PodGroup
		removeCompositePodGroup []*schedulingv1alpha3.CompositePodGroup
		wantPodGroups           []*schedulingv1beta1.PodGroup
		wantCompositePodGroups  []*schedulingv1alpha3.CompositePodGroup
		wantErr                 bool
	}{
		{
			name:            "Remove flat pod groups",
			initPodGroups:   []*schedulingv1beta1.PodGroup{pg1, pg2},
			removePodGroups: []*schedulingv1beta1.PodGroup{pg1, pg2},
			wantPodGroups:   nil,
		},
		{
			name:                    "Remove entire hierarchy of pod groups and composite pod groups",
			initPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			initCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			removePodGroups:         []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			removeCompositePodGroup: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			wantPodGroups:           nil,
			wantCompositePodGroups:  nil,
		},
		{
			name:                   "Remove child pod group unlinks it from parent composite pod group",
			initPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			initCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			removePodGroups:        []*schedulingv1beta1.PodGroup{leafPG1},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG2},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
		},
		{
			name:                   "Remove child pod group using lightweight lookup object unlinks it from parent composite pod group",
			initPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			initCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			removePodGroups: []*schedulingv1beta1.PodGroup{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: leafPG1.Namespace,
						Name:      leafPG1.Name,
					},
				},
			},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG2},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
		},
		{
			name:                   "Fallback on error when removing with lightweight object restores full original pod group",
			initPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1},
			initCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			removePodGroups: []*schedulingv1beta1.PodGroup{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: leafPG1.Namespace,
						Name:      leafPG1.Name,
					},
				},
				pg2,
			},
			wantPodGroups:          []*schedulingv1beta1.PodGroup{leafPG1},
			wantCompositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			wantErr:                true,
		},
		{
			name:                    "Remove parent composite pod group while child pod group remains",
			initPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			initCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{cpg1},
			removeCompositePodGroup: []*schedulingv1alpha3.CompositePodGroup{cpg1},
			wantPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			wantCompositePodGroups:  nil,
		},
		{
			name:            "Remove pod group while its pods still exist in snapshot keeps podGroupState",
			initPods:        []*v1.Pod{podInPG1},
			initPodGroups:   []*schedulingv1beta1.PodGroup{pg1},
			removePodGroups: []*schedulingv1beta1.PodGroup{pg1},
			wantPodGroups:   nil,
		},
		{
			name:            "Fallback on error when removing non-existent pod group restores previously removed pod groups",
			initPodGroups:   []*schedulingv1beta1.PodGroup{pg1},
			removePodGroups: []*schedulingv1beta1.PodGroup{pg1, pg2},
			wantPodGroups:   []*schedulingv1beta1.PodGroup{pg1},
			wantErr:         true,
		},
		{
			name:            "Fallback on error when removing same pod group twice in one call",
			initPodGroups:   []*schedulingv1beta1.PodGroup{pg1},
			removePodGroups: []*schedulingv1beta1.PodGroup{pg1, pg1},
			wantPodGroups:   []*schedulingv1beta1.PodGroup{pg1},
			wantErr:         true,
		},
		{
			name:                    "Fallback on error when removing non-existent composite pod group restores removed pod groups and composite pod groups",
			initPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			initCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			removePodGroups:         []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			removeCompositePodGroup: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			wantPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1, leafPG2},
			wantCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1},
			wantErr:                 true,
		},
		{
			name:                    "Fallback on error when removing same composite pod group twice in one call",
			initPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			initCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{cpg1},
			removePodGroups:         []*schedulingv1beta1.PodGroup{leafPG1},
			removeCompositePodGroup: []*schedulingv1alpha3.CompositePodGroup{cpg1, cpg1},
			wantPodGroups:           []*schedulingv1beta1.PodGroup{leafPG1},
			wantCompositePodGroups:  []*schedulingv1alpha3.CompositePodGroup{cpg1},
			wantErr:                 true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := cache.NewTestSnapshotWithCompositePodGroups(tc.initPods, nil, tc.initPodGroups, tc.initCompositePodGroups)
			cs := New(snap, nil)

			err := cs.Transaction(t.Context(), func() (TransactionResult, error) {
				for _, pg := range tc.removePodGroups {
					if err := cs.RemovePodGroup(t.Context(), pg); err != nil {
						return Revert, err
					}
				}
				for _, cpg := range tc.removeCompositePodGroup {
					if err := cs.RemoveCompositePodGroup(t.Context(), cpg); err != nil {
						return Revert, err
					}
				}
				return Commit, nil
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Remove operations error = %v, wantErr %v", err, tc.wantErr)
			}

			verifySnapshotPodGroups(t, snap, tc.wantPodGroups, tc.wantCompositePodGroups)

			if len(tc.initPods) > 0 {
				pgState, err := snap.PodGroupStates().Get(podInPG1.Namespace, "pg1")
				if err != nil {
					t.Fatalf("expected pod group state for pg1 to exist, got error: %v", err)
				}
				if pgState.AllPodsCount() != len(tc.initPods) {
					t.Errorf("expected AllPodsCount() = %d, got %d", len(tc.initPods), pgState.AllPodsCount())
				}
			}
		})
	}
}

type testVictim struct {
	name string
	pods []*v1.Pod
}

func (v *testVictim) Pods() []*v1.Pod {
	return v.pods
}

type cpuPreemptionFilter struct {
	minRequiredCPU int64
	preemptedCPU   int64
	activeVictims  map[PreemptionVictim]bool
	preemptCalls   int
	unpreemptCalls int
	mayFitCalls    int
}

func newCPUPreemptionFilter(minRequiredCPU int64) *cpuPreemptionFilter {
	return &cpuPreemptionFilter{
		minRequiredCPU: minRequiredCPU,
		activeVictims:  make(map[PreemptionVictim]bool),
	}
}

func (f *cpuPreemptionFilter) PreemptVictim(v PreemptionVictim) {
	f.preemptCalls++
	f.activeVictims[v] = true
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.preemptedCPU += cpu.Value()
		}
	}
}

func (f *cpuPreemptionFilter) UnpreemptVictim(v PreemptionVictim) {
	f.unpreemptCalls++
	delete(f.activeVictims, v)
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.preemptedCPU -= cpu.Value()
		}
	}
}

func (f *cpuPreemptionFilter) MayFit() bool {
	f.mayFitCalls++
	return f.preemptedCPU >= f.minRequiredCPU
}

func makeScheduledTestPod(name, nodeName, podGroupName, cpu string) *v1.Pod {
	p := testutils.MakePod(name, podGroupName, cpu)
	p.Spec.NodeName = nodeName
	return p
}

func victimNames(victims []PreemptionVictim) []string {
	if len(victims) == 0 {
		return nil
	}
	names := make([]string, 0, len(victims))
	for _, v := range victims {
		if tv, ok := v.(*testVictim); ok {
			names = append(names, tv.name)
		} else {
			names = append(names, fmt.Sprintf("%T", v))
		}
	}
	return names
}

func TestScheduleWorkload_Preemption(t *testing.T) {
	ctx := context.Background()

	node6CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "6",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPG := testutils.MakeGangPodGroup("gang-pg", "", 2)

	v1Pod2CPU := makeScheduledTestPod("v1-pod", "node1", "", "2")
	v2Pod2CPU := makeScheduledTestPod("v2-pod", "node1", "", "2")
	v3Pod2CPU := makeScheduledTestPod("v3-pod", "node1", "", "2")

	vSmallPod := makeScheduledTestPod("v-small-pod", "node1", "", "1")
	vMedPod := makeScheduledTestPod("v-med-pod", "node1", "", "3")
	vHighPod := makeScheduledTestPod("v-high-pod", "node1", "", "1")

	vMultiPod1 := makeScheduledTestPod("v-multi-1", "node1", "", "2")
	vMultiPod2 := makeScheduledTestPod("v-multi-2", "node1", "", "2")
	vMultiSmallPod1 := makeScheduledTestPod("v-multi-s1", "node1", "", "1")
	vMultiSmallPod2 := makeScheduledTestPod("v-multi-s2", "node1", "", "1")
	vSinglePod := makeScheduledTestPod("v-single-1", "node1", "", "4")

	vCommittedPod := makeScheduledTestPod("v-committed", "node1", "", "2")

	vMed2CPUPod := makeScheduledTestPod("v-med2-pod", "node1", "", "2")
	vLarge3CPUPod := makeScheduledTestPod("v-large3-pod", "node1", "", "3")

	tests := []struct {
		name                string
		nodes               []*v1.Node
		existingPods        []*v1.Pod
		workloadPods        []*v1.Pod
		buildOpts           func() (ScheduleWorkloadOptions, *cpuPreemptionFilter)
		wantStatusCode      fwk.Code
		wantScheduled       bool
		wantVictimNames     []string
		expectSnapshotState map[string]sets.Set[string]
	}{
		{
			name:         "(a) workload fits without preemption when sufficient capacity is free",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{v1Pod2CPU, v2Pod2CPU},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vic1 := &testVictim{name: "v1", pods: []*v1.Pod{v1Pod2CPU}}
				vic2 := &testVictim{name: "v2", pods: []*v1.Pod{v2Pod2CPU}}
				f := newCPUPreemptionFilter(2)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vic1, vic2}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Success,
			wantScheduled:   true,
			wantVictimNames: nil,
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v1-pod", "v2-pod", "w-pod1", "w-pod2"),
			},
		},
		{
			name:         "(b) workload fits after preempting subset of PotentialVictims and reprieves lower-priority victim",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vSmall := &testVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}}
				vMed := &testVictim{name: "vMed", pods: []*v1.Pod{vMedPod}}
				vHigh := &testVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}}
				f := newCPUPreemptionFilter(3)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vSmall, vMed, vHigh}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vMed"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-small-pod", "v-med-pod", "v-high-pod"),
			},
		},
		{
			name:         "(b-nil-filter) nil PreemptionFilter pulls incrementally one victim at a time and stops at minimal fitting prefix",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vSmall := &testVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}}
				vMed := &testVictim{name: "vMed", pods: []*v1.Pod{vMedPod}}
				vHigh := &testVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}}
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vSmall, vMed, vHigh}),
						PreemptionFilter: nil,
					},
				}, nil
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vMed"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-small-pod", "v-med-pod", "v-high-pod"),
			},
		},
		{
			name:         "(b-node-reject-reprieve) ShouldAttemptReprieval passes MayFit while reprieveVictim rejects at node Filter level",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMed2CPUPod, vLarge3CPUPod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vSmall := &testVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}}
				vMed := &testVictim{name: "vMed2", pods: []*v1.Pod{vMed2CPUPod}}
				vLarge := &testVictim{name: "vLarge3", pods: []*v1.Pod{vLarge3CPUPod}}
				// Filter requires 3 CPU, whereas node1 needs 4 CPU freed.
				// Try 1 pulls [vSmall(1), vMed2(2)] = 3 CPU -> MayFit=true, fails node fit.
				// Try 2 pulls [vSmall(1), vMed2(2), vLarge3(3)] = 6 CPU -> MayFit=true, succeeds node fit.
				// Reprieve pass probes vLarge3 (remaining 3 CPU >= 3 -> MayFit=true, rejected by node Filter),
				// then reprieves vMed2 (remaining 4 CPU >= 3 -> MayFit=true, accepted by node Filter),
				// then probes vSmall (remaining 3 CPU >= 3 -> MayFit=true, rejected by node Filter).
				f := newCPUPreemptionFilter(3)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vSmall, vMed, vLarge}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vLarge3", "vSmall"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-small-pod", "v-med2-pod", "v-large3-pod"),
			},
		},
		{
			name:         "(c1) multi-pod PreemptionVictim is preempted atomically",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{vMultiPod1, vMultiPod2, vSinglePod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vMulti := &testVictim{name: "vMulti", pods: []*v1.Pod{vMultiPod1, vMultiPod2}}
				vSingle := &testVictim{name: "vSingle", pods: []*v1.Pod{vSinglePod}}
				f := newCPUPreemptionFilter(4)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vMulti, vSingle}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vMulti"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-multi-1", "v-multi-2", "v-single-1"),
			},
		},
		{
			name:         "(c2) multi-pod PreemptionVictim is restored atomically when reprieved",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vMultiSmallPod1, vMultiSmallPod2, vSinglePod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vMulti := &testVictim{name: "vMulti", pods: []*v1.Pod{vMultiSmallPod1, vMultiSmallPod2}}
				vSingle := &testVictim{name: "vSingle", pods: []*v1.Pod{vSinglePod}}
				f := newCPUPreemptionFilter(4)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vMulti, vSingle}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vSingle"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-multi-s1", "v-multi-s2", "v-single-1"),
			},
		},
		{
			name:         "(d1) CommittedVictims suffice without touching PotentialVictims and are included in PreemptionVictims",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{vCommittedPod, v1Pod2CPU, v2Pod2CPU},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vCommitted := &testVictim{name: "vCommitted", pods: []*v1.Pod{vCommittedPod}}
				vic1 := &testVictim{name: "v1", pods: []*v1.Pod{v1Pod2CPU}}
				vic2 := &testVictim{name: "v2", pods: []*v1.Pod{v2Pod2CPU}}
				f := newCPUPreemptionFilter(0)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						CommittedVictims: []PreemptionVictim{vCommitted},
						PotentialVictims: slices.Values([]PreemptionVictim{vic1, vic2}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vCommitted"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-committed", "v1-pod", "v2-pod"),
			},
		},
		{
			name:         "(d2) CommittedVictims combined with PotentialVictims includes CommittedVictims in PreemptionVictims",
			nodes:        []*v1.Node{node8CPU},
			existingPods: []*v1.Pod{vCommittedPod, v1Pod2CPU, v2Pod2CPU, v3Pod2CPU},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vCommitted := &testVictim{name: "vCommitted", pods: []*v1.Pod{vCommittedPod}}
				vic1 := &testVictim{name: "v1", pods: []*v1.Pod{v1Pod2CPU}}
				vic2 := &testVictim{name: "v2", pods: []*v1.Pod{v2Pod2CPU}}
				vic3 := &testVictim{name: "v3", pods: []*v1.Pod{v3Pod2CPU}}
				f := newCPUPreemptionFilter(2)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						CommittedVictims: []PreemptionVictim{vCommitted},
						PotentialVictims: slices.Values([]PreemptionVictim{vic1, vic2, vic3}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vCommitted", "v1"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-committed", "v1-pod", "v2-pod", "v3-pod"),
			},
		},
		{
			name:         "(e) workload fails after exhausting all PotentialVictims and rolls back all snapshot mutations",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vCommittedPod, v1Pod2CPU, v2Pod2CPU},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-heavy1", "gang-pg", "4"),
				testutils.MakePod("w-heavy2", "gang-pg", "4"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vCommitted := &testVictim{name: "vCommitted", pods: []*v1.Pod{vCommittedPod}}
				vic1 := &testVictim{name: "v1", pods: []*v1.Pod{v1Pod2CPU}}
				vic2 := &testVictim{name: "v2", pods: []*v1.Pod{v2Pod2CPU}}
				f := newCPUPreemptionFilter(2)
				return ScheduleWorkloadOptions{
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						CommittedVictims: []PreemptionVictim{vCommitted},
						PotentialVictims: slices.Values([]PreemptionVictim{vic1, vic2}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   false,
			wantVictimNames: nil,
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-committed", "v1-pod", "v2-pod"),
			},
		},
		{
			name:         "(f) DryRun with preemption returns scheduled result and PreemptionVictims while reverting all snapshot mutations",
			nodes:        []*v1.Node{node6CPU},
			existingPods: []*v1.Pod{vSmallPod, vMedPod, vHighPod},
			workloadPods: []*v1.Pod{
				testutils.MakePod("w-pod1", "gang-pg", "2"),
				testutils.MakePod("w-pod2", "gang-pg", "2"),
			},
			buildOpts: func() (ScheduleWorkloadOptions, *cpuPreemptionFilter) {
				vSmall := &testVictim{name: "vSmall", pods: []*v1.Pod{vSmallPod}}
				vMed := &testVictim{name: "vMed", pods: []*v1.Pod{vMedPod}}
				vHigh := &testVictim{name: "vHigh", pods: []*v1.Pod{vHighPod}}
				f := newCPUPreemptionFilter(3)
				return ScheduleWorkloadOptions{
					CommonSchedulingOptions: CommonSchedulingOptions{DryRun: true},
					WorkloadPreemptionOptions: WorkloadPreemptionOptions{
						PotentialVictims: slices.Values([]PreemptionVictim{vSmall, vMed, vHigh}),
						PreemptionFilter: f,
					},
				}, f
			},
			wantStatusCode:  fwk.Unschedulable,
			wantScheduled:   true,
			wantVictimNames: []string{"vMed"},
			expectSnapshotState: map[string]sets.Set[string]{
				"node1": sets.New("v-small-pod", "v-med-pod", "v-high-pod"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
				ctx,
				t,
				tt.existingPods,
				tt.nodes,
				[]*schedulingv1beta1.PodGroup{gangPG},
				nil,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			cs := New(snap, profileMap)

			opts, filter := tt.buildOpts()
			result := cs.ScheduleWorkload(ctx, tt.workloadPods, opts)
			if result.Status.IsError() {
				t.Fatalf("ScheduleWorkload() unexpected error: %v", result.Status.AsError())
			}

			if result.Status.Code() != tt.wantStatusCode {
				t.Fatalf("ScheduleWorkload() Status.Code() = %v, want %v (status: %v)", result.Status.Code(), tt.wantStatusCode, result.Status)
			}

			gotVictims := victimNames(result.PreemptionVictims)
			if diff := cmp.Diff(tt.wantVictimNames, gotVictims, cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("PreemptionVictims mismatch (-want +got):\n%s", diff)
			}

			if tt.wantScheduled {
				if len(result.PodResults) != len(tt.workloadPods) {
					t.Fatalf("ScheduleWorkload() len(PodResults) = %d, want %d", len(result.PodResults), len(tt.workloadPods))
				}
				for _, pRes := range result.PodResults {
					if !pRes.Status.IsSuccess() {
						t.Errorf("pod %s expected success, got %v", pRes.Pod.Name, pRes.Status)
					}
					if pRes.SelectedNodeName != "node1" {
						t.Errorf("pod %s SelectedNodeName = %q, want \"node1\"", pRes.Pod.Name, pRes.SelectedNodeName)
					}
					if pRes.Pod.Spec.NodeName != "node1" {
						t.Errorf("pod %s Spec.NodeName = %q, want \"node1\"", pRes.Pod.Name, pRes.Pod.Spec.NodeName)
					}
				}
				if filter != nil {
					if len(filter.activeVictims) != len(result.PreemptionVictims) {
						t.Errorf("filter activeVictims count = %d, want %d", len(filter.activeVictims), len(result.PreemptionVictims))
					}
				}
			} else if len(result.PodResults) != 0 {
				t.Errorf("expected empty PodResults on failure, got %v", result.PodResults)
			}

			ft.VerifySnapshot(t, snap, tt.expectSnapshotState)
		})
	}
}
