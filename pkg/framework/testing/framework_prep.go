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

package testing

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"sigs.k8s.io/scheduler-library/pkg/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

func SetupSnapshotTest(ctx context.Context, pods []*v1.Pod, nodes []*v1.Node) (*upstreamsync.ProfileMap, *cache.Snapshot, error) {
	framework.InitMetricsOnce()
	client := fake.NewClientset()
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			return nil, nil, err
		}
	}

	prof := schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "PrioritySort"},
						},
					},
					PreFilter: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "NodeResourcesFit"},
						},
					},
					Filter: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "NodeResourcesFit"},
						},
					},
					Bind: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "DefaultBinder"},
						},
					},
				},
				PluginConfig: []schedulerapi.PluginConfig{
					{
						Name: "NodeResourcesFit",
						Args: &schedulerapi.NodeResourcesFitArgs{
							ScoringStrategy: &schedulerapi.ScoringStrategy{
								Type: schedulerapi.LeastAllocated,
							},
						},
					},
				},
			},
		},
	}

	informerFactory := informers.NewSharedInformerFactory(client, 0)
	snap := cache.NewSnapshot(pods, nodes)
	comps, err := upstreamsync.NewFrameworkComponents(ctx, client, informerFactory, upstreamsync.WithProfiles(prof.Profiles...))
	if err != nil {
		return nil, nil, err
	}
	informerFactory.StartWithContext(ctx)
	informerFactory.WaitForCacheSyncWithContext(ctx)

	profileMap, err := upstreamsync.NewFrameworkMap(ctx, comps, framework.DiscardRecorderFactory, snap)
	if err != nil {
		return nil, nil, err
	}
	framework.ApplySimulationNeutralizers(profileMap)

	return profileMap, snap, nil
}

// SetupSnapshotTestWithPodGroups populates the snapshot with nodes, pods, podgroups, and composite podgroups,
// enables the feature gates for workload scheduling, and returns scheduler profile and snapshot.
func SetupSnapshotTestWithPodGroups(
	t testing.TB,
	ctx context.Context,
	pods []*v1.Pod,
	nodes []*v1.Node,
	podGroups []*schedulingv1beta1.PodGroup,
	compositePodGroups []*schedulingv1alpha3.CompositePodGroup,
) (*upstreamsync.ProfileMap, *cache.Snapshot, error) {
	framework.InitMetricsOnce()
	client := fake.NewClientset()
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			return nil, nil, err
		}
	}

	prof := schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					PreFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Filter:    schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
				},
				PluginConfig: []schedulerapi.PluginConfig{
					{
						Name: "NodeResourcesFit",
						Args: &schedulerapi.NodeResourcesFitArgs{
							ScoringStrategy: &schedulerapi.ScoringStrategy{
								Type: schedulerapi.LeastAllocated,
							},
						},
					},
				},
			},
		},
	}

	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.TopologyAwareWorkloadScheduling: true,
		features.GenericWorkload:                 true,
		features.CompositePodGroup:               true,
	})

	snap := cache.NewTestSnapshotWithCompositePodGroups(pods, nodes, podGroups, compositePodGroups)
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	comps, err := upstreamsync.NewFrameworkComponents(ctx, client, informerFactory, upstreamsync.WithProfiles(prof.Profiles...))
	if err != nil {
		return nil, nil, err
	}
	informerFactory.StartWithContext(ctx)
	informerFactory.WaitForCacheSyncWithContext(ctx)

	profileMap, err := upstreamsync.NewFrameworkMap(ctx, comps, framework.DiscardRecorderFactory, snap)
	if err != nil {
		return nil, nil, err
	}
	framework.ApplySimulationNeutralizers(profileMap)

	return profileMap, snap, nil
}
