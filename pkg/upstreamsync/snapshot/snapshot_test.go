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
	fwk "k8s.io/kube-scheduler/framework"
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
		result, err := sc.cs.ScheduleWorkload(sc.ctx, pods, opts)
		if wantSuccess {
			if err != nil {
				t.Fatalf("ScheduleWorkload(%v) unexpected error: %v", podNames, err)
			}
			if !result.Status.IsSuccess() {
				t.Fatalf("ScheduleWorkload(%v) unexpected failure status: %v", podNames, result.Status)
			}
			for i, res := range result.PodResults {
				if !res.Status.IsSuccess() {
					t.Fatalf("ScheduleWorkload(%v) result[%d] unexpected failure status: %v", podNames, i, res.Status)
				}
			}
		} else if err == nil {
			if result.Status.IsSuccess() {
				t.Fatalf("ScheduleWorkload(%v) expected failure status, but succeeded: %v", podNames, result)
			}
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

			result, err := cs.ScheduleWorkload(ctx, tt.pods, tt.opts)
			if (err != nil) != tt.expectErr {
				t.Fatalf("ScheduleWorkload() error = %v, expectErr %v", err, tt.expectErr)
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

type testPreemptionVictim struct {
	name string
	pods []*v1.Pod
}

func (v *testPreemptionVictim) Pods() []*v1.Pod {
	return v.pods
}

type cpuTrackingPreemptionFilter struct {
	requiredCPU    int64
	freedCPU       int64
	preemptCalls   int
	unpreemptCalls int
	mayFitCalls    int
}

func (f *cpuTrackingPreemptionFilter) PreemptVictim(v PreemptionVictim) {
	f.preemptCalls++
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.freedCPU += cpu.Value()
		}
	}
}

func (f *cpuTrackingPreemptionFilter) UnpreemptVictim(v PreemptionVictim) {
	f.unpreemptCalls++
	for _, p := range v.Pods() {
		for _, c := range p.Spec.Containers {
			cpu := c.Resources.Requests[v1.ResourceCPU]
			f.freedCPU -= cpu.Value()
		}
	}
}

func (f *cpuTrackingPreemptionFilter) MayFit() bool {
	f.mayFitCalls++
	return f.freedCPU >= f.requiredCPU
}

func makeAssignedPod(name, nodeName, cpu string) *v1.Pod {
	p := testutils.MakePod(name, "", cpu)
	p.Spec.NodeName = nodeName
	return p
}

func TestScheduleWorkload_Preemption(t *testing.T) {
	ctx := context.Background()

	node4CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node5CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "5",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node6CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "6",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node8CPU := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "8",
		v1.ResourceMemory: "8Gi",
		v1.ResourcePods:   "10",
	}).Obj()
	node2_4CPU := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{
		v1.ResourceCPU:    "4",
		v1.ResourceMemory: "4Gi",
		v1.ResourcePods:   "10",
	}).Obj()

	gangPG2 := testutils.MakeGangPodGroup("gang-pg", "", 2)
	gangPG1 := testutils.MakeGangPodGroup("gang-pg-1", "", 1)

	t.Run("Stage 0 - fits without preemption leaves PotentialVictims untouched", func(t *testing.T) {
		v1Pod := makeAssignedPod("v1", "node1", "2")
		vic1 := &testPreemptionVictim{name: "v1", pods: []*v1.Pod{v1Pod}}
		pulled := 0
		seq := func(yield func(PreemptionVictim) bool) {
			pulled++
			yield(vic1)
		}
		filter := &cpuTrackingPreemptionFilter{requiredCPU: 4}

		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(ctx, t, []*v1.Pod{v1Pod}, []*v1.Node{node8CPU}, []*schedulingv1beta1.PodGroup{gangPG2}, nil)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		wPod1 := testutils.MakePod("w1", "gang-pg", "2")
		wPod2 := testutils.MakePod("w2", "gang-pg", "2")
		opts := ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				PotentialVictims: seq,
				PreemptionFilter: filter,
			},
		}

		res, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod1, wPod2}, opts)
		if err != nil {
			t.Fatalf("ScheduleWorkload failed: %v", err)
		}
		if !res.Status.IsSuccess() {
			t.Fatalf("Expected success, got %v", res.Status)
		}
		if pulled != 0 {
			t.Errorf("Expected 0 victims pulled from iterator, got %d", pulled)
		}
		if filter.preemptCalls != 0 || filter.unpreemptCalls != 0 {
			t.Errorf("Expected 0 filter calls, got preempt=%d unpreempt=%d", filter.preemptCalls, filter.unpreemptCalls)
		}
		if len(res.PreemptionVictims) != 0 {
			t.Errorf("Expected 0 PreemptionVictims, got %d", len(res.PreemptionVictims))
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("v1", "w1", "w2")})
	})

	t.Run("Stage 1a - CommittedVictims pre-removed, never reprieved, and notified to PreemptionFilter", func(t *testing.T) {
		cv1Pod := makeAssignedPod("cv1", "node1", "2")
		cv2Pod := makeAssignedPod("cv2", "node1", "2")
		pv1Pod := makeAssignedPod("pv1", "node1", "1")
		cv1 := &testPreemptionVictim{name: "cv1", pods: []*v1.Pod{cv1Pod}}
		cv2 := &testPreemptionVictim{name: "cv2", pods: []*v1.Pod{cv2Pod}}
		pv1 := &testPreemptionVictim{name: "pv1", pods: []*v1.Pod{pv1Pod}}

		filter := &cpuTrackingPreemptionFilter{requiredCPU: 2}
		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(ctx, t, []*v1.Pod{cv1Pod, cv2Pod, pv1Pod}, []*v1.Node{node5CPU}, []*schedulingv1beta1.PodGroup{gangPG2}, nil)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		// The workload needs 2 CPU. Both committed victims cv1 and cv2 stay removed.
		wPod1 := testutils.MakePod("w1", "gang-pg", "1")
		wPod2 := testutils.MakePod("w2", "gang-pg", "1")
		opts := ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				CommittedVictims: []PreemptionVictim{cv1, cv2},
				PotentialVictims: slices.Values([]PreemptionVictim{pv1}),
				PreemptionFilter: filter,
			},
		}

		res, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod1, wPod2}, opts)
		if err != nil {
			t.Fatalf("ScheduleWorkload failed: %v", err)
		}
		if !res.Status.IsSuccess() {
			t.Fatalf("Expected success, got %v", res.Status)
		}
		if filter.preemptCalls != 2 || filter.freedCPU != 4 {
			t.Errorf("Expected PreemptionFilter notified for 2 CommittedVictims (4 CPU), got calls=%d freedCPU=%d", filter.preemptCalls, filter.freedCPU)
		}
		if len(res.PreemptionVictims) != 0 {
			t.Errorf("Expected 0 newly selected PreemptionVictims from PotentialVictims, got %d", len(res.PreemptionVictims))
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("pv1", "w1", "w2")})
	})

	t.Run("Stage 1a & 1b - CommittedVictims combined with PotentialVictims, DryRun and failure restore CommittedVictims", func(t *testing.T) {
		cv1Pod := makeAssignedPod("cv1", "node1", "2")
		pv1Pod := makeAssignedPod("pv1", "node1", "2")
		pv2Pod := makeAssignedPod("pv2", "node1", "2")
		cv1 := &testPreemptionVictim{name: "cv1", pods: []*v1.Pod{cv1Pod}}
		pv1 := &testPreemptionVictim{name: "pv1", pods: []*v1.Pod{pv1Pod}}
		pv2 := &testPreemptionVictim{name: "pv2", pods: []*v1.Pod{pv2Pod}}

		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(ctx, t, []*v1.Pod{cv1Pod, pv1Pod, pv2Pod}, []*v1.Node{node6CPU}, []*schedulingv1beta1.PodGroup{gangPG2}, nil)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		// DryRun with a 4 CPU workload requires cv1 and pv1. DryRun restores both victims afterward.
		wPod1 := testutils.MakePod("w1", "gang-pg", "2")
		wPod2 := testutils.MakePod("w2", "gang-pg", "2")
		dryRunRes, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod1, wPod2}, ScheduleWorkloadOptions{
			CommonSchedulingOptions: CommonSchedulingOptions{DryRun: true},
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				CommittedVictims: []PreemptionVictim{cv1},
				PotentialVictims: slices.Values([]PreemptionVictim{pv1, pv2}),
			},
		})
		if err != nil || !dryRunRes.Status.IsSuccess() {
			t.Fatalf("DryRun ScheduleWorkload failed: err=%v status=%v", err, dryRunRes.Status)
		}
		if len(dryRunRes.PreemptionVictims) != 1 || dryRunRes.PreemptionVictims[0] != pv1 {
			t.Errorf("Expected PreemptionVictims=[pv1], got %v", dryRunRes.PreemptionVictims)
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("cv1", "pv1", "pv2")})

		// A failure with an 8 CPU workload on a 6 CPU node restores cv1.
		heavy1 := testutils.MakePod("h1", "gang-pg", "4")
		heavy2 := testutils.MakePod("h2", "gang-pg", "4")
		failRes, err := cs.ScheduleWorkload(ctx, []*v1.Pod{heavy1, heavy2}, ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				CommittedVictims: []PreemptionVictim{cv1},
				PotentialVictims: slices.Values([]PreemptionVictim{pv1, pv2}),
			},
		})
		if err != nil || failRes.Status.IsSuccess() {
			t.Fatalf("Expected unschedulable failure, got err=%v status=%v", err, failRes.Status)
		}
		if len(failRes.PreemptionVictims) != 0 {
			t.Errorf("Expected empty PreemptionVictims on failure, got %v", failRes.PreemptionVictims)
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("cv1", "pv1", "pv2")})
	})

	t.Run("Stage 1b & Stage 2 - incremental prefix search stops at first fitting prefix and reprieves in reverse order", func(t *testing.T) {
		// node1 (5 CPU) has v1 (1 CPU), v2 (1 CPU), v3 (2 CPU), and v4 (1 CPU).
		// The iterator yields v4 (1 CPU) after v3.
		// The workload needs 3 CPU (w1=1, w2=2).
		// Prefix [v1, v2, v3] frees 4 CPU and fits the workload.
		// Stage 2 checks v3, v2, and v1 in reverse order.
		// Stage 2 reprieves v2 and keeps v1 and v3 as victims.
		v1Pod := makeAssignedPod("v1", "node1", "1")
		v2Pod := makeAssignedPod("v2", "node1", "1")
		v3Pod := makeAssignedPod("v3", "node1", "2")
		v4Pod := makeAssignedPod("v4", "node1", "1")
		vic1 := &testPreemptionVictim{name: "v1", pods: []*v1.Pod{v1Pod}}
		vic2 := &testPreemptionVictim{name: "v2", pods: []*v1.Pod{v2Pod}}
		vic3 := &testPreemptionVictim{name: "v3", pods: []*v1.Pod{v3Pod}}
		vic4 := &testPreemptionVictim{name: "v4", pods: []*v1.Pod{v4Pod}}

		pulled := 0
		seq := func(yield func(PreemptionVictim) bool) {
			for _, v := range []PreemptionVictim{vic1, vic2, vic3, vic4} {
				pulled++
				if !yield(v) {
					return
				}
			}
		}
		filter := &cpuTrackingPreemptionFilter{requiredCPU: 3}

		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
			ctx, t,
			[]*v1.Pod{v1Pod, v2Pod, v3Pod, v4Pod},
			[]*v1.Node{node5CPU},
			[]*schedulingv1beta1.PodGroup{gangPG2},
			nil,
		)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		wPod1 := testutils.MakePod("w1", "gang-pg", "1")
		wPod2 := testutils.MakePod("w2", "gang-pg", "2")
		res, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod1, wPod2}, ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				PotentialVictims: seq,
				PreemptionFilter: filter,
			},
		})
		if err != nil || !res.Status.IsSuccess() {
			t.Fatalf("ScheduleWorkload failed: err=%v status=%v", err, res.Status)
		}
		if pulled != 3 {
			t.Errorf("Expected iterator to stop after pulling 3 victims, pulled %d", pulled)
		}
		if len(res.PreemptionVictims) != 2 || res.PreemptionVictims[0] != vic1 || res.PreemptionVictims[1] != vic3 {
			t.Errorf("Expected PreemptionVictims=[vic1, vic3] (with vic2 reprieved), got %v", res.PreemptionVictims)
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{
			"node1": sets.New("v2", "v4", "w1", "w2"),
		})
	})

	t.Run("Stage 1b rollback & Stage 2 PreemptionFilter probe-and-restore when node Filter rejects reprieval", func(t *testing.T) {
		// node1 (4 CPU) hosts v1 (2 CPU) and v3 (2 CPU).
		// node2 (4 CPU) hosts v2 (2 CPU) and blocker (2 CPU).
		// Prefix [v1, v2] passes MayFit() but fails node placement and rolls back.
		// Prefix [v1, v2, v3] frees 4 CPU on node1 and fits w1 (4 CPU).
		// Stage 2 probes v3, restores PreemptVictim(v3), and then reprieves v2 on node2.
		v1Pod := makeAssignedPod("v1", "node1", "2")
		v2Pod := makeAssignedPod("v2", "node2", "2")
		v3Pod := makeAssignedPod("v3", "node1", "2")
		blockerPod := makeAssignedPod("blocker", "node2", "2")
		vic1 := &testPreemptionVictim{name: "v1", pods: []*v1.Pod{v1Pod}}
		vic2 := &testPreemptionVictim{name: "v2", pods: []*v1.Pod{v2Pod}}
		vic3 := &testPreemptionVictim{name: "v3", pods: []*v1.Pod{v3Pod}}

		filter := &cpuTrackingPreemptionFilter{requiredCPU: 4}
		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
			ctx, t,
			[]*v1.Pod{v1Pod, v2Pod, v3Pod, blockerPod},
			[]*v1.Node{node4CPU, node2_4CPU},
			[]*schedulingv1beta1.PodGroup{gangPG1},
			nil,
		)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		wPod := testutils.MakePod("w1", "gang-pg-1", "4")
		res, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod}, ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				PotentialVictims: slices.Values([]PreemptionVictim{vic1, vic2, vic3}),
				PreemptionFilter: filter,
			},
		})
		if err != nil || !res.Status.IsSuccess() {
			t.Fatalf("ScheduleWorkload failed: err=%v status=%v", err, res.Status)
		}
		if len(res.PreemptionVictims) != 2 || res.PreemptionVictims[0] != vic1 || res.PreemptionVictims[1] != vic3 {
			t.Errorf("Expected PreemptionVictims=[vic1, vic3] (vic2 reprieved), got %v", res.PreemptionVictims)
		}
		if filter.freedCPU != 4 {
			t.Errorf("Expected final filter.freedCPU = 4, got %d", filter.freedCPU)
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{
			"node1": sets.New("w1"),
			"node2": sets.New("v2", "blocker"),
		})
	})

	t.Run("Multi-pod atomic PreemptionVictim", func(t *testing.T) {
		// vMulti has 2 pods (vp1a=2 CPU, vp1b=2 CPU) on node1 (4 CPU).
		// Because vMulti is an atomic victim, Stage 2 preempts both pods together.
		vp1a := makeAssignedPod("vp1a", "node1", "2")
		vp1b := makeAssignedPod("vp1b", "node1", "2")
		vMulti := &testPreemptionVictim{name: "vMulti", pods: []*v1.Pod{vp1a, vp1b}}

		profileMap, snap, err := ft.SetupSnapshotTestWithPodGroups(
			ctx, t,
			[]*v1.Pod{vp1a, vp1b},
			[]*v1.Node{node4CPU},
			[]*schedulingv1beta1.PodGroup{gangPG2},
			nil,
		)
		if err != nil {
			t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
		}
		cs := New(snap, profileMap)

		wPod1 := testutils.MakePod("w1", "gang-pg", "1")
		wPod2 := testutils.MakePod("w2", "gang-pg", "2")
		res, err := cs.ScheduleWorkload(ctx, []*v1.Pod{wPod1, wPod2}, ScheduleWorkloadOptions{
			WorkloadPreemptionOptions: WorkloadPreemptionOptions{
				PotentialVictims: slices.Values([]PreemptionVictim{vMulti}),
			},
		})
		if err != nil || !res.Status.IsSuccess() {
			t.Fatalf("ScheduleWorkload failed: err=%v status=%v", err, res.Status)
		}
		if len(res.PreemptionVictims) != 1 || res.PreemptionVictims[0] != vMulti {
			t.Errorf("Expected PreemptionVictims=[vMulti], got %v", res.PreemptionVictims)
		}
		ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{
			"node1": sets.New("w1", "w2"),
		})
	})
}
