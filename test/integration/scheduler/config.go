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

package scheduler

import (
	v1 "k8s.io/api/core/v1"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
)

type configOpt func(*schedulerapi.Plugins)

// withTopologyPlacementGenerator enables the TopologyPlacementGenerator and PodGroupPodsCount
// plugins in PlacementGenerate and PlacementScore for pod group scheduling, matching upstream's
// default topology-aware workload scheduling plugin configuration.
func withTopologyPlacementGenerator(plugins *schedulerapi.Plugins) {
	plugins.PlacementGenerate = schedulerapi.PluginSet{
		Enabled: []schedulerapi.Plugin{{Name: "TopologyPlacementGenerator"}},
	}
	plugins.PlacementScore = schedulerapi.PluginSet{
		Enabled: []schedulerapi.Plugin{{Name: "PodGroupPodsCount", Weight: 1}},
	}
}

// withNodeAffinity enables the NodeAffinity plugin across filter and score extension points.
func withNodeAffinity(plugins *schedulerapi.Plugins) {
	plugins.PreFilter.Enabled = append(plugins.PreFilter.Enabled, schedulerapi.Plugin{Name: "NodeAffinity"})
	plugins.Filter.Enabled = append(plugins.Filter.Enabled, schedulerapi.Plugin{Name: "NodeAffinity"})
	plugins.PreScore.Enabled = append(plugins.PreScore.Enabled, schedulerapi.Plugin{Name: "NodeAffinity"})
	plugins.Score.Enabled = append(plugins.Score.Enabled, schedulerapi.Plugin{Name: "NodeAffinity", Weight: 2})
}

// newKubeSchedulerConfig returns the scheduler configuration shared by the simulator integration
// tests: the smallest set of plugins that still exercises a real filter and bind cycle.
// Additional plugins can be enabled via optional configOpt mutators.
func newKubeSchedulerConfig(opts ...configOpt) *schedulerapi.KubeSchedulerConfiguration {
	cfg := &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					MultiPoint:         schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "GangScheduling"}}},
					QueueSort:          schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					PreFilter:          schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					Filter:             schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "NodeResourcesFit"}}},
					PodGroupPostFilter: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultPreemption"}}},
					Bind:               schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
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
					{
						Name: "DefaultPreemption",
						Args: &schedulerapi.DefaultPreemptionArgs{
							MinCandidateNodesPercentage: 10,
							MinCandidateNodesAbsolute:   100,
						},
					},
					{
						Name: "NodeAffinity",
						Args: &schedulerapi.NodeAffinityArgs{},
					},
				},
			},
		},
	}

	for _, opt := range opts {
		opt(cfg.Profiles[0].Plugins)
	}

	return cfg
}
