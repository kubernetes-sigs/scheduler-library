package simulator

import (
	"context"

	v1 "k8s.io/api/core/v1"
	"scheduler-library/pkg/snapshot"
	"scheduler-library/pkg/state"
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

