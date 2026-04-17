// Package state provides implementations for cluster state management.
package state

import (
	"context"

	"scheduler-library/pkg/snapshot"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// clusterState implements ClusterState interface.
type clusterState struct {
	internalCache cache.Cache
	frameworks    map[string]schedulerframework.Framework
}

// NewClusterState creates a new ClusterState with an internal Kubernetes scheduler cache and frameworks.
func NewClusterState(c cache.Cache, frameworks map[string]schedulerframework.Framework) ClusterState {
	return &clusterState{
		internalCache: c,
		frameworks:    frameworks,
	}
}

// AddPod adds a pod to the internal cache.
func (s *clusterState) AddPod(ctx context.Context, pod *v1.Pod) {
	logger := klog.FromContext(ctx)
	if err := s.internalCache.AddPod(logger, pod); err != nil {
		logger.Error(err, "Failed to add pod to cache", "pod", pod.Name)
	}
}

// RemovePod removes a pod from the internal cache.
func (s *clusterState) RemovePod(ctx context.Context, pod *v1.Pod) {
	logger := klog.FromContext(ctx)
	if err := s.internalCache.RemovePod(logger, pod); err != nil {
		logger.Error(err, "Failed to remove pod from cache", "pod", pod.Name)
	}
}

// AddNode adds a node to the internal cache.
func (s *clusterState) AddNode(ctx context.Context, node *v1.Node) {
	logger := klog.FromContext(ctx)
	s.internalCache.AddNode(logger, node)
}

// RemoveNode removes a node from the internal cache.
func (s *clusterState) RemoveNode(ctx context.Context, node *v1.Node) {
	logger := klog.FromContext(ctx)
	if err := s.internalCache.RemoveNode(logger, node); err != nil {
		logger.Error(err, "Failed to remove node from cache", "node", node.Name)
	}
}

// Snapshot constructs a snapshot based on the current state of the cluster.
func (s *clusterState) Snapshot(ctx context.Context) snapshot.ClusterSnapshot {
	logger := klog.FromContext(ctx)
	newEmptySnapshot := cache.NewEmptySnapshot()
	if err := s.internalCache.UpdateSnapshot(logger, newEmptySnapshot); err != nil {
		logger.Error(err, "Failed to update snapshot")
	}
	return snapshot.NewClusterSnapshot(newEmptySnapshot, s.frameworks)
}
