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
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/snapshot"
)

func TestClusterState_StaleSnapshotRejectsMutation(t *testing.T) {
	ctx := t.Context()
	logger := klog.FromContext(ctx)

	sharedSnap := cache.NewEmptySnapshot()
	state := New(cache.New(ctx, nil, false), newDummyProfileMap(), sharedSnap)
	state.Cache.AddNode(logger, st.MakeNode().Name("node1").Obj())
	victim := st.MakePod().Name("victim").Namespace("default").UID("victim").Node("node1").Obj()
	victim2 := st.MakePod().Name("victim2").Namespace("default").UID("victim2").Node("node1").Obj()
	for _, p := range []*v1.Pod{victim, victim2} {
		if err := state.Cache.AddPod(logger, p); err != nil {
			t.Fatalf("AddPod() error = %v", err)
		}
	}

	stale, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	// Take a handle while stale is still current, for the Unpreempt case below.
	handle, err := stale.PreemptPods(ctx, []*v1.Pod{victim})
	if err != nil {
		t.Fatalf("PreemptPods() setup error = %v", err)
	}

	// A newer snapshot supersedes stale.
	current, err := state.Snapshot(logger)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}

	// The handle belongs to stale, so the snapshot that superseded it will not
	// replay it either. Only checking the receiver would let this through: the
	// revert closes over the shared state, and current starts its own preemption
	// versions at zero, which is what the handle recorded.
	if _, err := current.Unpreempt(handle); err == nil {
		t.Error("Unpreempt() on the current snapshot with a handle from the stale one = nil, want error")
	}

	// Every mutation entry point on the stale snapshot is rejected.
	if _, err := stale.PreemptPods(ctx, []*v1.Pod{victim2}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale PreemptPods should be rejected, got %v", err)
	}
	if _, err := stale.Unpreempt(handle); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale Unpreempt should be rejected, got %v", err)
	}
	err = stale.Transaction(ctx, func() (snapshot.TransactionResult, error) { return snapshot.Commit, nil })
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale Transaction should be rejected, got %v", err)
	}

	// The current snapshot still mutates normally.
	if _, err := current.PreemptPods(ctx, []*v1.Pod{victim2}); err != nil {
		t.Fatalf("current PreemptPods should succeed, got %v", err)
	}
}
