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

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"sigs.k8s.io/scheduler-library/pkg/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

func SetupSnapshotTest(ctx context.Context, pods []*v1.Pod, nodes []*v1.Node, opts ...upstreamsync.Option) (*upstreamsync.ProfileMap, *cache.Snapshot, error) {
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

	snap := cache.NewSnapshot(pods, nodes)
	profileMap, err := framework.NewProfileMap(ctx,
		client,
		nil,
		snap,
		&prof,
		opts...,
	)
	if err != nil {
		return nil, nil, err
	}

	return profileMap, snap, nil
}
