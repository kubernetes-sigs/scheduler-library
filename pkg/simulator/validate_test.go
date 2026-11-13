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

	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/utils/ptr"
)

func TestValidateKubeSchedulerConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *schedulerapi.KubeSchedulerConfiguration
		wantErr string
	}{
		{
			name: "unset and boundary values are valid",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				PercentageOfNodesToScore: ptr.To[int32](100),
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "unset"},
					{SchedulerName: "zero", PercentageOfNodesToScore: ptr.To[int32](0)},
					{SchedulerName: "hundred", PercentageOfNodesToScore: ptr.To[int32](100)},
				},
			},
		},
		{
			name: "negative global value",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				PercentageOfNodesToScore: ptr.To[int32](-1),
			},
			wantErr: "percentageOfNodesToScore: Invalid value: -1: not in valid range [0-100]",
		},
		{
			name: "global value above 100",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				PercentageOfNodesToScore: ptr.To[int32](101),
			},
			wantErr: "percentageOfNodesToScore: Invalid value: 101: not in valid range [0-100]",
		},
		{
			name: "negative profile value",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "negative", PercentageOfNodesToScore: ptr.To[int32](-1)},
				},
			},
			wantErr: "profiles[0].percentageOfNodesToScore: Invalid value: -1: not in valid range [0-100]",
		},
		{
			name: "profile value above 100",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "above", PercentageOfNodesToScore: ptr.To[int32](101)},
				},
			},
			wantErr: "profiles[0].percentageOfNodesToScore: Invalid value: 101: not in valid range [0-100]",
		},
		{
			name: "aggregates invalid global and profile values",
			cfg: &schedulerapi.KubeSchedulerConfiguration{
				PercentageOfNodesToScore: ptr.To[int32](-5),
				Profiles: []schedulerapi.KubeSchedulerProfile{
					{SchedulerName: "negative", PercentageOfNodesToScore: ptr.To[int32](-1)},
					{SchedulerName: "valid", PercentageOfNodesToScore: ptr.To[int32](50)},
					{SchedulerName: "above", PercentageOfNodesToScore: ptr.To[int32](101)},
				},
			},
			wantErr: "[profiles[0].percentageOfNodesToScore: Invalid value: -1: not in valid range [0-100], " +
				"profiles[2].percentageOfNodesToScore: Invalid value: 101: not in valid range [0-100], " +
				"percentageOfNodesToScore: Invalid value: -5: not in valid range [0-100]]",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateKubeSchedulerConfiguration(tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateKubeSchedulerConfiguration() unexpected error = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tc.wantErr {
				t.Errorf("ValidateKubeSchedulerConfiguration() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
