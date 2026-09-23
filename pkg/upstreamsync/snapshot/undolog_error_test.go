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
	"errors"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	ft "sigs.k8s.io/scheduler-library/pkg/framework/testing"
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
	// If the retained history is ever shorter than the version counter, an old target
	// must be rejected rather than unwound into a slice panic.
	ul.undoOperations = nil
	if err := ul.restoreState(0); err == nil {
		t.Fatal("restoreState past retained history should return an error, not panic")
	}
}

func TestTransactionSurfacesRollbackError(t *testing.T) {
	undoBoom := errors.New("undo-boom")
	cs := New(cache.NewEmptySnapshot(), nil)
	err := cs.Transaction(context.Background(), func() (TransactionResult, error) {
		cs.undoLog.registerOperation(func() error { return undoBoom })
		return Revert, nil
	})
	if !errors.Is(err, undoBoom) {
		t.Fatalf("Transaction should surface a failed rollback via errors.Is, got %v", err)
	}
}

func TestUnpreemptSurfacesRevertError(t *testing.T) {
	revertBoom := errors.New("revert-boom")
	cs := New(cache.NewEmptySnapshot(), nil)
	u := &Unpreemption{
		revertFn:               func() error { return revertBoom },
		validPreemptionVersion: cs.stateVersionForPreemption,
	}
	if _, err := cs.Unpreempt(u); !errors.Is(err, revertBoom) {
		t.Fatalf("Unpreempt should surface a failed revert via errors.Is, got %v", err)
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
}

func TestUnpreemptRejectsUninitializedHandle(t *testing.T) {
	cs := New(cache.NewEmptySnapshot(), nil)
	// A zero-value handle validates against a fresh snapshot's version 0, so it must be
	// rejected on its nil revert rather than dereferencing it.
	if _, err := cs.Unpreempt(&Unpreemption{}); err == nil {
		t.Fatal("Unpreempt of an uninitialized handle should return an error, not panic")
	}
}

func TestPreemptPodsPartialRemovalFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	// ghost has a valid node name but is absent from the snapshot, so it clears the
	// up-front validation and fails during removal, forcing a rollback of pod1.
	ghost := st.MakePod().Name("ghost").Namespace("default").UID("uid-ghost").Node("node1").Obj()

	cs, snap, _ := setupSnapshotTest(t, ctx, []*v1.Node{node1}, []*v1.Pod{pod1})

	if _, err := cs.PreemptPods(ctx, []*v1.Pod{pod1, ghost}); err == nil {
		t.Fatal("PreemptPods should fail when a pod is missing from the snapshot")
	}
	ft.VerifySnapshot(t, snap, map[string]sets.Set[string]{"node1": sets.New("pod1")})
}

func TestPreemptPodsRevertUsesRemovalTimeCopy(t *testing.T) {
	ctx := context.Background()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	node2 := st.MakeNode().Name("node2").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()

	cs, snap, _ := setupSnapshotTest(t, ctx, []*v1.Node{node1, node2}, []*v1.Pod{pod1})

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
	ctx := context.Background()
	node1 := st.MakeNode().Name("node1").Capacity(map[v1.ResourceName]string{v1.ResourcePods: "10"}).Obj()
	pod1 := st.MakePod().Name("pod1").Namespace("default").UID("uid-pod1").Node("node1").Obj()
	cs, _, _ := setupSnapshotTest(t, ctx, []*v1.Node{node1}, []*v1.Pod{pod1})

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
