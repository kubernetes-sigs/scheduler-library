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

package framework_test

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"sigs.k8s.io/scheduler-library/pkg/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

// stubDRAManager stands in for the informer-backed manager. The test only checks which
// manager the frameworks were given, so none of the accessors are ever called.
type stubDRAManager struct{}

var _ fwk.SharedDRAManager = &stubDRAManager{}

func (s *stubDRAManager) ResourceClaims() fwk.ResourceClaimTracker     { return nil }
func (s *stubDRAManager) ResourceSlices() fwk.ResourceSliceLister      { return nil }
func (s *stubDRAManager) DeviceClasses() fwk.DeviceClassLister         { return nil }
func (s *stubDRAManager) DeviceClassResolver() fwk.DeviceClassResolver { return nil }

func minimalProfile() *schedulerapi.KubeSchedulerConfiguration {
	return &schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: v1.DefaultSchedulerName,
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
					Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
				},
			},
		},
	}
}

func TestNewProfileMapSharedDRAManager(t *testing.T) {
	// DynamicResourceAllocation is locked on from 1.37, so there is no gate-off case to cover.
	stub := &stubDRAManager{}

	cases := map[string]struct {
		opts     []upstreamsync.Option
		wantStub bool
	}{
		"a supplied manager reaches the frameworks": {
			opts:     []upstreamsync.Option{upstreamsync.WithSharedDRAManager(stub)},
			wantStub: true,
		},
		"without one the informer-backed manager is built": {},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			profiles, err := framework.NewProfileMap(t.Context(), fake.NewClientset(), nil,
				cache.NewSnapshot(nil, nil), minimalProfile(), tc.opts...)
			if err != nil {
				t.Fatalf("NewProfileMap failed: %v", err)
			}

			got := profiles.Map[v1.DefaultSchedulerName].SharedDRAManager()
			switch {
			case tc.wantStub && got != fwk.SharedDRAManager(stub):
				t.Errorf("SharedDRAManager() = %v, want the supplied stub", got)
			case !tc.wantStub && got == nil:
				t.Error("SharedDRAManager() = nil, want the informer-backed manager")
			case !tc.wantStub && got == fwk.SharedDRAManager(stub):
				t.Error("SharedDRAManager() returned the stub, want the informer-backed manager")
			}
		})
	}
}
