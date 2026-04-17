package state

import (
	"context"

	"scheduler-library/pkg/snapshot"

	v1 "k8s.io/api/core/v1"
)

// ClusterState is meant to reflect the runtime state of the cluster, updated as resources are added and deleted.
// Any "fake" / simulated changes should happen on ClusterSnapshot instead.
type ClusterState interface {
	// AddPod adds a pod to the cluster state.
	AddPod(ctx context.Context, pod *v1.Pod)

	// RemovePod removes a pod from the cluster state.
	RemovePod(ctx context.Context, pod *v1.Pod)

	// AddNode adds a node to the cluster state.
	// This is different from snapshot's AddNodes as ClusterState is meant to use "real" nodes and they don't support snapshot transactions or interweaving with other snapshot operations (like scheduling).
	AddNode(ctx context.Context, node *v1.Node)

	// RemoveNode removes a node from the cluster state.
	RemoveNode(ctx context.Context, node *v1.Node)

	// Snapshot constructs a snapshot based on the current state of the cluster.
	// Typically faster than NewClusterSnapshot(pods, nodes) if the ClusterState is pre-populated.
	Snapshot(ctx context.Context) snapshot.ClusterSnapshot
}
