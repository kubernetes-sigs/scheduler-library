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
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

func TestUndoLogRestoreStatePropagatesError(t *testing.T) {
	boomB := errors.New("boom-b")
	boomC := errors.New("boom-c")
	var ul undoLog
	var order []string
	ul.registerOperation(func() error { order = append(order, "a"); return nil })
	ul.registerOperation(func() error { order = append(order, "b"); return boomB })
	ul.registerOperation(func() error { order = append(order, "c"); return boomC })

	err := ul.restoreState(0)
	// Both failures are preserved and discoverable with errors.Is.
	if !errors.Is(err, boomB) || !errors.Is(err, boomC) {
		t.Fatalf("restoreState should join every undo error, got %v", err)
	}
	// Every undo still runs, in LIFO order, even though two failed.
	if want := []string{"c", "b", "a"}; !slices.Equal(order, want) {
		t.Fatalf("undo order = %v, want %v", order, want)
	}
	if ul.stateVersion != 0 {
		t.Fatalf("stateVersion = %d, want 0 after unwinding every operation", ul.stateVersion)
	}
}

func TestUndoLogRestoreStateToNonZeroTarget(t *testing.T) {
	boomC := errors.New("boom-c")
	var ul undoLog
	var order []string
	ul.registerOperation(func() error { order = append(order, "a"); return nil })
	ul.registerOperation(func() error { order = append(order, "b"); return nil })
	ul.registerOperation(func() error { order = append(order, "c"); return boomC })

	// Unwind only back to version 1; the failing "c" is still surfaced.
	if err := ul.restoreState(1); !errors.Is(err, boomC) {
		t.Fatalf("restoreState(1) should surface boom-c, got %v", err)
	}
	if want := []string{"c", "b"}; !slices.Equal(order, want) {
		t.Fatalf("undo order = %v, want %v", order, want)
	}
	if ul.stateVersion != 1 {
		t.Fatalf("stateVersion = %d, want 1", ul.stateVersion)
	}
}

func TestUndoLogRestoreStateRejectsUnreachableTarget(t *testing.T) {
	var ul undoLog
	ul.registerOperation(func() error { return nil })

	// A future version cannot be reached and must not run undo on an empty log.
	if err := ul.restoreState(5); err == nil {
		t.Fatal("restoreState to a future version should return an error, not panic")
	}
}

func TestTransactionSurfacesRollbackError(t *testing.T) {
	undoBoom := errors.New("undo-boom")
	cs := New(cache.NewEmptySnapshot(), nil)
	err := cs.Transaction(t.Context(), func() (TransactionResult, error) {
		cs.undoLog.registerOperation(func() error { return undoBoom })
		return Revert, nil
	})
	if !errors.Is(err, undoBoom) {
		t.Fatalf("Transaction should surface a failed rollback via errors.Is, got %v", err)
	}
	if err := cs.ResetMutations(); !errors.Is(err, undoBoom) {
		t.Fatalf("ResetMutations after a failed rollback: want the rollback error, got %v", err)
	}
}

func TestUnpreemptSurfacesRevertError(t *testing.T) {
	revertBoom := errors.New("revert-boom")
	cs := New(cache.NewEmptySnapshot(), nil)
	u := &Unpreemption{
		revertFn:               func() error { return revertBoom },
		validPreemptionVersion: cs.stateVersionForPreemption,
	}
	sibling := &Unpreemption{
		revertFn:               func() error { return nil },
		validPreemptionVersion: cs.stateVersionForPreemption,
	}
	if _, err := cs.Unpreempt(u); !errors.Is(err, revertBoom) {
		t.Fatalf("Unpreempt should surface a failed revert via errors.Is, got %v", err)
	}
	for _, h := range []*Unpreemption{u, sibling} {
		if _, err := cs.Unpreempt(h); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("Unpreempt after a failed revert: want invalid, got %v", err)
		}
	}
}

func TestResetMutationsSurfacesRestoreError(t *testing.T) {
	undoBoom := errors.New("undo-boom")
	cs := New(cache.NewEmptySnapshot(), nil)
	cs.undoLog.registerOperation(func() error { return undoBoom })

	before := cs.stateVersionForPreemption
	if err := cs.ResetMutations(); !errors.Is(err, undoBoom) {
		t.Fatalf("ResetMutations should surface a failed reset via errors.Is, got %v", err)
	}
	// A failed reset leaves the snapshot inconsistent, so outstanding handles must be invalidated.
	if cs.stateVersionForPreemption == before {
		t.Fatal("ResetMutations should advance the preemption version after a failed reset")
	}
	// The failed undo has left the log, so a second reset must not report success.
	if err := cs.ResetMutations(); !errors.Is(err, undoBoom) {
		t.Fatalf("second ResetMutations: want the earlier failure, got %v", err)
	}
}

func TestPodGroupUndoSurfacesError(t *testing.T) {
	ctx := t.Context()
	pg := testutils.MakeBasicPodGroup("pg", "")
	cpg := testutils.MakeBasicCompositePodGroup("cpg", "")
	tests := []struct {
		name string
		// seed adds pg and cpg before the test, for the cases that remove them.
		seed bool
		// mutate makes the logged change; sabotage changes the snapshot behind the log so the undo fails.
		mutate   func(cs *ClusterSnapshot) error
		sabotage func(cs *ClusterSnapshot) error
	}{
		{
			name:     "AddPodGroup",
			mutate:   func(cs *ClusterSnapshot) error { return cs.AddPodGroup(ctx, pg) },
			sabotage: func(cs *ClusterSnapshot) error { _, err := cs.removePodGroup(pg); return err },
		},
		{
			name:     "RemovePodGroup",
			seed:     true,
			mutate:   func(cs *ClusterSnapshot) error { return cs.RemovePodGroup(ctx, pg) },
			sabotage: func(cs *ClusterSnapshot) error { return cs.addPodGroup(pg.DeepCopy()) },
		},
		{
			name:     "AddCompositePodGroup",
			mutate:   func(cs *ClusterSnapshot) error { return cs.AddCompositePodGroup(ctx, cpg) },
			sabotage: func(cs *ClusterSnapshot) error { _, err := cs.removeCompositePodGroup(cpg); return err },
		},
		{
			name:     "RemoveCompositePodGroup",
			seed:     true,
			mutate:   func(cs *ClusterSnapshot) error { return cs.RemoveCompositePodGroup(ctx, cpg) },
			sabotage: func(cs *ClusterSnapshot) error { return cs.addCompositePodGroup(cpg.DeepCopy()) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := New(cache.NewEmptySnapshot(), nil)
			if tt.seed {
				if err := errors.Join(cs.addPodGroup(pg.DeepCopy()), cs.addCompositePodGroup(cpg.DeepCopy())); err != nil {
					t.Fatalf("seeding the snapshot: %v", err)
				}
			}
			if err := tt.mutate(cs); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if err := tt.sabotage(cs); err != nil {
				t.Fatalf("sabotage: %v", err)
			}
			if err := cs.ResetMutations(); err == nil || !strings.Contains(err.Error(), "during state revert") {
				t.Fatalf("ResetMutations: want the failed %s undo, got %v", tt.name, err)
			}
		})
	}
}

// failingUndoFilter registers an undo that fails while ScheduleWorkload runs.
type failingUndoFilter struct {
	cs  *ClusterSnapshot
	err error
}

func (f *failingUndoFilter) PreemptVictim(PreemptionVictim)   {}
func (f *failingUndoFilter) UnpreemptVictim(PreemptionVictim) {}
func (f *failingUndoFilter) MayFit() bool {
	f.cs.undoLog.registerOperation(func() error { return f.err })
	return true
}

func TestScheduleWorkloadSurfacesRollbackError(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload: true,
	})
	ctx := t.Context()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourcePods: "10"}).Obj()
	profiles, snap, err := ft.SetupSnapshotTestWithPodGroups(ctx, t, nil, []*v1.Node{node1},
		[]*schedulingv1beta1.PodGroup{testutils.MakeBasicPodGroup("pg", "")}, nil)
	if err != nil {
		t.Fatalf("SetupSnapshotTestWithPodGroups: %v", err)
	}
	cs := New(snap, profiles)

	undoBoom := errors.New("undo-boom")
	// The workload fits, so a dry run rolls back a successful pass.
	opts := ScheduleWorkloadOptions{WorkloadPreemptionOptions: WorkloadPreemptionOptions{
		PreemptionFilter: &failingUndoFilter{cs: cs, err: undoBoom},
	}}
	opts.DryRun = true
	result := cs.ScheduleWorkload(ctx, []*v1.Pod{testutils.MakePod("p1", "pg", "1")}, opts)
	if !result.Status.IsError() || !errors.Is(result.Status.AsError(), undoBoom) {
		t.Fatalf("ScheduleWorkload: want an error status carrying the failed rollback, got %v", result.Status)
	}
	if len(result.PodResults) != 0 || result.WaitingForPreemption {
		t.Errorf("an error result must not carry pod results, got %+v", result)
	}
	if err := cs.ResetMutations(); !errors.Is(err, undoBoom) {
		t.Fatalf("ResetMutations after the failed rollback: want the rollback error, got %v", err)
	}
}

func TestUnpreemptRejectsUninitializedHandle(t *testing.T) {
	cs := New(cache.NewEmptySnapshot(), nil)
	// A zero-value handle validates against a fresh snapshot's version 0, so it must be
	// rejected on its nil revert rather than dereferencing it.
	if _, err := cs.Unpreempt(&Unpreemption{}); err == nil {
		t.Fatal("Unpreempt of an uninitialized handle should return an error, not panic")
	}
}

func TestPreemptPodsRevertUsesRemovalTimeCopy(t *testing.T) {
	ctx := t.Context()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()

	cs, snap, _ := setupSnapshotTest(ctx, t, []*v1.Node{node1, node2}, []*v1.Pod{pod1})

	victim := pod1.DeepCopy()
	handle, err := cs.PreemptPods(ctx, []*v1.Pod{victim})
	if err != nil {
		t.Fatalf("PreemptPods: %v", err)
	}
	// The caller mutates its pod after preemption; the revert must ignore that and
	// restore pod1 to node1, not to node2.
	victim.Spec.NodeName = "node2"
	if _, err := cs.Unpreempt(handle); err != nil {
		t.Fatalf("Unpreempt: %v", err)
	}
	ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("pod1"), "node2": sets.New[string]()})
}

func TestTransactionFailedRollbackInvalidatesPriorHandles(t *testing.T) {
	ctx := t.Context()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	cs, _, _ := setupSnapshotTest(ctx, t, []*v1.Node{node1}, []*v1.Pod{pod1})

	// A handle created before the transaction.
	handle, err := cs.PreemptPods(ctx, []*v1.Pod{pod1})
	if err != nil {
		t.Fatalf("PreemptPods: %v", err)
	}

	undoBoom := errors.New("undo-boom")
	err = cs.Transaction(ctx, func() (TransactionResult, error) {
		cs.undoLog.registerOperation(func() error { return undoBoom })
		return Revert, nil
	})
	if !errors.Is(err, undoBoom) {
		t.Fatalf("Transaction should surface the failed rollback, got %v", err)
	}

	// A failed rollback leaves the snapshot inconsistent; the handle from before the
	// transaction must not revalidate against it.
	if _, err := cs.Unpreempt(handle); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("stale handle after failed rollback: want invalid, got %v", err)
	}
}

func TestFailedRollbackInsideTransactionKeepsPriorHandlesInvalid(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.GenericWorkload: true,
	})
	ctx := t.Context()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourceCPU: "4", v1.ResourcePods: "10"}).Obj()
	victim := makeScheduledTestPod("victim", "node1", "", "1")
	profiles, snap, err := ft.SetupSnapshotTestWithPodGroups(ctx, t, []*v1.Pod{victim}, []*v1.Node{node1},
		[]*schedulingv1beta1.PodGroup{testutils.MakeBasicPodGroup("pg", "")}, nil)
	if err != nil {
		t.Fatalf("SetupSnapshotTestWithPodGroups: %v", err)
	}
	cs := New(snap, profiles)
	handle, err := cs.PreemptPods(ctx, []*v1.Pod{victim})
	if err != nil {
		t.Fatalf("PreemptPods: %v", err)
	}

	undoBoom := errors.New("undo-boom")
	// The transaction's own rollback succeeds, but a rollback inside it has failed.
	err = cs.Transaction(ctx, func() (TransactionResult, error) {
		opts := ScheduleWorkloadOptions{WorkloadPreemptionOptions: WorkloadPreemptionOptions{
			PreemptionFilter: &failingUndoFilter{cs: cs, err: undoBoom},
		}}
		opts.DryRun = true
		cs.ScheduleWorkload(ctx, []*v1.Pod{testutils.MakePod("p1", "pg", "1")}, opts)
		return Revert, nil
	})
	if err != nil {
		t.Fatalf("Transaction: %v", err)
	}
	if _, err := cs.Unpreempt(handle); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("pre-transaction handle after a failed rollback: want invalid, got %v", err)
	}
}
