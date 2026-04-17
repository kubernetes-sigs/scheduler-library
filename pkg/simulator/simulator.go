package simulator

import (
	"context"

	"scheduler-library/pkg/snapshot"
	"scheduler-library/pkg/state"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"

	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
	plugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

type schedulingSimulator struct {
	cfg             *schedulerapi.KubeSchedulerConfiguration
	informerFactory informers.SharedInformerFactory
}

// NewSchedulingSimulator creates a new SchedulingSimulator.
func NewSchedulingSimulator(cfg *schedulerapi.KubeSchedulerConfiguration, informerFactory informers.SharedInformerFactory) SchedulingSimulator {
	return &schedulingSimulator{
		cfg:             cfg,
		informerFactory: informerFactory,
	}
}


// NewClusterState initializes a new runtime cluster state.
func (s *schedulingSimulator) NewClusterState(ctx context.Context) (state.ClusterState, error) {
	internalCache := cache.New(ctx, nil, false)
	// Create a snapshot for framework creation
	snap := cache.NewEmptySnapshot()

	registry := plugins.NewInTreeRegistry()

	frameworks := make(map[string]schedulerframework.Framework)
	for _, profile := range s.cfg.Profiles {
		profile := profile // avoid capturing loop variable
		f, err := frameworkruntime.NewFramework(ctx, registry, &profile,
			frameworkruntime.WithSnapshotSharedLister(snap),
			frameworkruntime.WithInformerFactory(s.informerFactory),
		)
		if err != nil {
			return nil, err
		}
		frameworks[profile.SchedulerName] = f
	}

	return state.NewClusterState(internalCache, frameworks), nil
}


// NewClusterSnapshot initializes a new snapshot with the provided pods and nodes.
func (s *schedulingSimulator) NewClusterSnapshot(ctx context.Context, pods []*v1.Pod, nodes []*v1.Node) (snapshot.ClusterSnapshot, error) {
	snap := cache.NewSnapshot(pods, nodes)

	registry := plugins.NewInTreeRegistry()

	frameworks := make(map[string]schedulerframework.Framework)
	for _, profile := range s.cfg.Profiles {
		profile := profile // avoid capturing loop variable
		f, err := frameworkruntime.NewFramework(ctx, registry, &profile,
			frameworkruntime.WithSnapshotSharedLister(snap),
			frameworkruntime.WithInformerFactory(s.informerFactory),
		)
		if err != nil {
			return nil, err
		}
		frameworks[profile.SchedulerName] = f
	}

	return snapshot.NewClusterSnapshot(snap, frameworks), nil
}

