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
	"context"

	"sigs.k8s.io/scheduler-library/pkg/snapshot"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// clusterState implements ClusterState interface.
var _ ClusterState = (*clusterState)(nil)

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
	var snap *cache.Snapshot
	for _, fwk := range s.frameworks {
		if fwk != nil {
			if lister := fwk.SnapshotSharedLister(); lister != nil {
				if cacheSnap, ok := lister.(*cache.Snapshot); ok {
					if err := s.internalCache.UpdateSnapshot(logger, cacheSnap); err != nil {
						logger.Error(err, "Failed to update snapshot")
					}
					snap = cacheSnap
				}
			}
		}
	}
	if snap == nil {
		snap = cache.NewEmptySnapshot()
		if err := s.internalCache.UpdateSnapshot(logger, snap); err != nil {
			logger.Error(err, "Failed to update snapshot")
		}
	}
	return snapshot.NewClusterSnapshot(snap, s.frameworks)
}
