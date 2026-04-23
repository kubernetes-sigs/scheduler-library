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

package simulator

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/scheduler-library/pkg/snapshot"
	"sigs.k8s.io/scheduler-library/pkg/state"
)

// SchedulingSimulator is the entry point for running scheduling simulations.
type SchedulingSimulator interface {
	// NewClusterState initializes a new runtime cluster state.
	NewClusterState(ctx context.Context) (state.ClusterState, error)

	// NewClusterSnapshot initializes a new snapshot with the provided pods and nodes.
	// Typically slower than ClusterState.Snapshot, unless ClusterState is not updated as new resources are added and deleted.
	// The provided pods and nodes are the complete set of pods and nodes that the user wants to include in the initial snapshot.
	// Those should represent the "real" state.
	NewClusterSnapshot(ctx context.Context, pods []*v1.Pod, nodes []*v1.Node) (snapshot.ClusterSnapshot, error)
}
