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
	"k8s.io/kubernetes/pkg/scheduler/framework"
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
		results, err := sc.cs.ScheduleWorkload(sc.ctx, pods, opts)
		if wantSuccess {
			if err != nil {
				t.Fatalf("ScheduleWorkload(%v) unexpected error: %v", podNames, err)
			}
			for i, res := range results {
				if !res.Status.IsSuccess() {
					t.Fatalf("ScheduleWorkload(%v) result[%d] unexpected failure status: %v", podNames, i, res.Status)
				}
			}
		} else {
			if err == nil {
				allSucceeded := len(results) > 0
				for _, res := range results {
					if !res.Status.IsSuccess() {
						allSucceeded = false
						break
					}
				}
				if allSucceeded {
					t.Fatalf("ScheduleWorkload(%v) expected failure status, but all succeeded: %v", podNames, results)
				}
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

			cs, snap, _ := setupSnapshotTest(t, ctx, nodesForTest, assignedPods)
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

	cs, _, _ := setupSnapshotTest(t, context.Background(), []*v1.Node{node1, node2}, nil)

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
	basePod := func() *st.PodWrapper {
		return st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1")
	}
	emptyNodeStatus := framework.NewNodeToStatus(map[string]*fwk.Status{}, nil)

	tests := []struct {
		name           string
		candidateNodes []string
		pod            *v1.Pod
		// schedulingGates puts .spec.schedulingGates on the pod, which the SchedulingGates
		// PreEnqueue plugin rejects.
		expectNodes       []string
		expectedDiagnosis *framework.Diagnosis
		expectErr         bool
	}{
		{
			name:           "Success - all nodes eligible",
			candidateNodes: []string{"node1", "node2"},
			expectNodes:    []string{"node1", "node2"},
			pod:            basePod().Obj(),
			expectedDiagnosis: &framework.Diagnosis{
				NodeToStatus:         emptyNodeStatus,
				UnschedulablePlugins: sets.New[string](),
			},
		},
		{
			name:           "Error - unknown scheduler name",
			candidateNodes: []string{"node1"},
			pod:            basePod().SchedulerName("unknown-scheduler").Obj(),
			expectErr:      true,
		},
		{
			name:           "Success - empty candidate list returns empty result",
			candidateNodes: []string{},
			pod:            basePod().Obj(),
			expectNodes:    nil,
		},
		{
			name:           "Rejected - insufficient cpu",
			candidateNodes: []string{"node1"},
			pod: basePod().Req(map[v1.ResourceName]string{
				v1.ResourceCPU: "1",
			}).Obj(),
			expectNodes: []string{},
			expectedDiagnosis: &framework.Diagnosis{
				NodeToStatus: framework.NewNodeToStatus(map[string]*fwk.Status{
					"node1": fwk.NewStatus(fwk.UnschedulableAndUnresolvable, "Insufficient cpu").WithPlugin("NodeResourcesFit"),
				}, fwk.NewStatus(fwk.UnschedulableAndUnresolvable)),
				UnschedulablePlugins: sets.New("NodeResourcesFit"),
			},
		},
		{
			name:           "Gated - pod carrying scheduling gates never reaches PreFilter",
			candidateNodes: []string{"node1", "node2"},
			pod:            basePod().SchedulingGates([]string{"example.com/gate"}).Obj(),
			expectNodes:    []string{},
			expectedDiagnosis: &framework.Diagnosis{
				NodeToStatus:         emptyNodeStatus,
				UnschedulablePlugins: sets.New("SchedulingGates"),
				PreFilterMsg:         "waiting for scheduling gates: [example.com/gate]",
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

			cs, _, _ := setupSnapshotTest(t, ctx, snapshotNodes, nil)

			placement, err := cs.MakePlacement(tc.candidateNodes)
			if err != nil {
				t.Fatalf("MakePlacement() error = %v", err)
			}

			nodes, diagnosis, err := cs.CanSchedulePod(ctx, tc.pod, placement)
			if err != nil && !tc.expectErr {
				t.Fatalf("CanSchedulePod() error = %v", err)
			}

			if diff := cmp.Diff(tc.expectNodes, nodes, cmpopts.EquateEmpty(), cmpopts.SortSlices(func(x, y string) bool { return x < y })); diff != "" {
				t.Errorf("Unexpected nodes (-want +got):\n%s", diff)
			}

			if diff := cmp.Diff(tc.expectedDiagnosis, diagnosis, nodeToStatusCmpOpt); diff != "" {
				t.Errorf("Unexpected diagnosis (-want +got):\n%s", diff)
			}
		})
	}
}

var nodeToStatusCmpOpt = cmp.Comparer(func(x, y *framework.NodeToStatus) bool {
	if x.Len() != y.Len() {
		return false
	}

	isEqual := true

	x.ForEachExplicitNode(func(nodeName string, xStatus *fwk.Status) {
		if !isEqual {
			return
		}

		yStatus := y.Get(nodeName)
		if yStatus == nil || xStatus.Code() != yStatus.Code() || xStatus.Message() != yStatus.Message() {
			isEqual = false
		}
	})

	return isEqual
})

var scheduleResultCmpOpts = []cmp.Option{
	cmpopts.EquateEmpty(),
	cmp.Comparer(func(x, y *fwk.Status) bool {
		return x.Code() == y.Code()
	}),
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
	// gatedPod and ungatedPod differ only by .spec.schedulingGates, which is all the SchedulingGates
	// PreEnqueue plugin looks at.
	gatedPod := st.MakePod().Name("gatedPod").Namespace("default").UID("uid-gated").SchedulingGates([]string{"example.com/gate"}).Obj()
	ungatedPod := st.MakePod().Name("ungatedPod").Namespace("default").UID("uid-ungated").Obj()

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
			name:           "PreEnqueue gate - gated pod is rejected before the scheduling cycle",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{gatedPod},
			candidateNodes: []string{"node1"},
			opts:           SchedulePodsOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              gatedPod,
					SelectedNodeName: "",
					Status:           fwk.NewStatus(fwk.UnschedulableAndUnresolvable),
					GatingPlugin:     "SchedulingGates",
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": nil},
		},
		{
			name:           "PreEnqueue gate - the same pod without the gate is scheduled",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{ungatedPod},
			candidateNodes: []string{"node1"},
			opts:           SchedulePodsOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(ungatedPod, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("ungatedPod")},
		},
		{
			name:           "PreEnqueue gate - StopOnFailure stops the loop at the gated pod",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1, gatedPod, pod2},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(false, true),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:          gatedPod,
					Status:       fwk.NewStatus(fwk.UnschedulableAndUnresolvable),
					GatingPlugin: "SchedulingGates",
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1")},
		},
		{
			name:           "PreEnqueue gate - without StopOnFailure the loop goes past the gated pod",
			nodes:          []*v1.Node{node1},
			pods:           []*v1.Pod{pod1, gatedPod, pod2},
			candidateNodes: []string{"node1"},
			opts:           NewSchedulePodsOptions(false, false),
			expectResults: []SchedulingResult{
				{
					Pod:              onNode(pod1, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
				{
					Pod:          gatedPod,
					Status:       fwk.NewStatus(fwk.UnschedulableAndUnresolvable),
					GatingPlugin: "SchedulingGates",
				},
				{
					Pod:              onNode(pod2, "node1"),
					SelectedNodeName: "node1",
					Status:           fwk.NewStatus(fwk.Success),
				},
			},
			expectSnapshotState: map[string]sets.Set[string]{"node1": sets.New("pod1", "pod2")},
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

			cs, snap, _ := setupSnapshotTest(t, ctx, tc.nodes, nil)

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
			}

			// A gated pod never enters a scheduling cycle, so it is left unassigned.
			for _, res := range results {
				if res.GatingPlugin == "" {
					continue
				}
				if res.SelectedNodeName != "" {
					t.Errorf("expected gated pod %s to have no selected node, got %q", res.Pod.Name, res.SelectedNodeName)
				}
				if res.Pod.Spec.NodeName != "" {
					t.Errorf("expected gated pod %s to keep an empty NodeName, got %q", res.Pod.Name, res.Pod.Spec.NodeName)
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

	cs, snap, _ := setupSnapshotTest(t, ctx, []*v1.Node{node}, nil)
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
	gatedTemplate := &v1.PodTemplateSpec{
		Spec: v1.PodSpec{
			SchedulingGates: []v1.PodSchedulingGate{{Name: "example.com/gate"}},
		},
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
			name:           "PreEnqueue gate - gated template yields a single result and no further pod",
			template:       gatedTemplate,
			nodes:          []*v1.Node{node1},
			candidateNodes: []string{"node1"},
			maxPods:        5,
			opts:           SchedulePodsByTemplateOptions{},
			expectResults: []SchedulingResult{
				{
					Pod:          generatedPod(0),
					Status:       fwk.NewStatus(fwk.UnschedulableAndUnresolvable),
					GatingPlugin: "SchedulingGates",
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

			cs, snap, _ := setupSnapshotTest(t, ctx, tc.nodes, nil)

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

	cs, snap, _ := setupSnapshotTest(t, ctx, []*v1.Node{node1, node2}, []*v1.Pod{pod1, pod2})

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
				t,
				ctx,
				nil,
				tt.nodes,
				tt.podGroups,
				tt.compositePodGroups,
			)
			if err != nil {
				t.Fatalf("SetupSnapshotTestWithPodGroups failed: %v", err)
			}
			cs := New(snap, profileMap)

			results, err := cs.ScheduleWorkload(ctx, tt.pods, tt.opts)
			if (err != nil) != tt.expectErr {
				t.Fatalf("ScheduleWorkload() error = %v, expectErr %v", err, tt.expectErr)
			}

			if !tt.expectErr && len(tt.expectResults) > 0 {
				if len(results) != len(tt.expectResults) {
					t.Fatalf("ScheduleWorkload() got %d results, want %d", len(results), len(tt.expectResults))
				}
				for i := range results {
					if results[i].SelectedNodeName != tt.expectResults[i].SelectedNodeName {
						t.Errorf("result[%d] SelectedNodeName = %q, want %q", i, results[i].SelectedNodeName, tt.expectResults[i].SelectedNodeName)
					}
					if results[i].Status.IsSuccess() != tt.expectResults[i].Status.IsSuccess() {
						t.Errorf("result[%d] Status.IsSuccess = %v, want %v", i, results[i].Status.IsSuccess(), tt.expectResults[i].Status.IsSuccess())
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
				t,
				ctx,
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
