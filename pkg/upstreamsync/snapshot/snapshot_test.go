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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
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

			cs, _, _ := setupSnapshotTest(t, ctx, snapshotNodes, nil)

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
					Pod:              pod1,
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
					Pod:              pod1,
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
					Pod:              pod1,
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:              pod2,
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
					Pod:              pod1,
					Status:           fwk.NewStatus(fwk.Success),
					SelectedNodeName: "node1",
				},
				{
					Pod:              pod2,
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
					Pod:              pod1,
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
					Pod:              pod1,
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
					Pod:              pod1,
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

			results, err := cs.SchedulePods(ctx, tc.pods, placement, tc.opts)
			if (err != nil) != tc.expectErr {
				t.Fatalf("SchedulePods() error = %v, expectErr %v", err, tc.expectErr)
			}

			if diff := cmp.Diff(tc.expectResults, results, scheduleResultCmpOpts...); diff != "" {
				t.Errorf("Unexpected scheduling results (-want +got):\n%s", diff)
			}

			// SchedulePods reflects the selected node on the pods it was given.
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
