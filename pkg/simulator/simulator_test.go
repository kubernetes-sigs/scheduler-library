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
	"testing"

	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

func init() {
	metrics.Register()
}

func TestNewSchedulingSimulator(t *testing.T) {
	cfg := &schedulerapi.KubeSchedulerConfiguration{}
	informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
	sim := NewSchedulingSimulator(cfg, informerFactory)
	if sim == nil {
		t.Fatal("Expected simulator to be non-nil")
	}
}

func TestNewClusterState(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "default-scheduler"},
				},
			},
			expectErr: false,
		},
		{
			name: "error with invalid profile (non-existent plugin)",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "invalid-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "NonExistentPlugin"},
								},
							},
						},
					},
				},
			},
			expectErr: true,
		},
		{
			name: "success with multiple profiles",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "profile-1"},
					{SchedulerName: "profile-2"},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
			sim := NewSchedulingSimulator(tc.cfg, informerFactory)
			ctx := t.Context()

			state, err := sim.NewClusterState(ctx)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterState err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && state == nil {
				t.Fatal("Expected state to be non-nil")
			}
		})
	}

}

func TestNewClusterSnapshot(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *schedulerapi.KubeSchedulerConfiguration
		expectErr bool
	}{
		{
			name: "success with default profile",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "default-scheduler"},
				},
			},
			expectErr: false,
		},
		{
			name: "error with invalid profile (non-existent plugin)",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{
						SchedulerName: "invalid-scheduler",
						Plugins: &schedulerapi.Plugins{
							QueueSort: schedulerapi.PluginSet{
								Enabled: []schedulerapi.Plugin{
									{Name: "NonExistentPlugin"},
								},
							},
						},
					},
				},
			},
			expectErr: true,
		},
		{
			name: "success with multiple profiles",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "profile-1"},
					{SchedulerName: "profile-2"},
				},
			},
			expectErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			informerFactory := informers.NewSharedInformerFactory(fake.NewClientset(), 0)
			sim := NewSchedulingSimulator(tc.cfg, informerFactory)
			ctx := t.Context()

			snapshot, err := sim.NewClusterSnapshot(ctx, nil, nil)
			if (err != nil) != tc.expectErr {
				t.Errorf("NewClusterSnapshot err = %v, expectErr %v", err, tc.expectErr)
			}
			if !tc.expectErr && snapshot == nil {
				t.Fatal("Expected snapshot to be non-nil")
			}
		})
	}

}
