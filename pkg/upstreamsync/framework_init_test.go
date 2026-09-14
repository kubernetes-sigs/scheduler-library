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

package upstreamsync

/*
Note: Upstream Kubernetes (pkg/scheduler/framework_init.go) does not have a corresponding
framework_init_test.go. The unit tests below verify the adapted FrameworkComponents and
NewFrameworkMap functionality in scheduler-library.
*/

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// verifyErr verifies that the given error is the error expected by the test case.
func verifyErr(wantErrMsg string, err error) error {
	if wantErrMsg != "" && err == nil {
		return fmt.Errorf("want error, got nil")
	}

	if wantErrMsg == "" && err != nil {
		return fmt.Errorf("want no error, got %v", err)
	}

	if err != nil {
		if want, got := wantErrMsg, err.Error(); want != got {
			return fmt.Errorf("incorrect error: want: %q got: %q", want, got)
		}
	}
	return nil
}

func fakeRecorderFactory(string) events.EventRecorderLogger {
	return &events.FakeRecorder{}
}

func TestNewFrameworkComponents(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	comps, err := NewFrameworkComponents(ctx, client, informerFactory)
	if err != nil {
		t.Fatalf("NewFrameworkComponents failed: %v", err)
	}
	if comps == nil {
		t.Fatal("expected non-nil FrameworkComponents")
	}
	if len(comps.options.profiles) == 0 {
		t.Fatal("expected default profile to be configured")
	}
}

func TestNewFrameworkMap(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	cfg := schedulerapi.KubeSchedulerConfiguration{
		Profiles: []schedulerapi.KubeSchedulerProfile{
			{
				SchedulerName: "default-scheduler",
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "PrioritySort"},
						},
					},
					Bind: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "DefaultBinder"},
						},
					},
				},
			},
			{
				SchedulerName: "custom-scheduler",
				Plugins: &schedulerapi.Plugins{
					QueueSort: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "PrioritySort"},
						},
					},
					Bind: schedulerapi.PluginSet{
						Enabled: []schedulerapi.Plugin{
							{Name: "DefaultBinder"},
						},
					},
				},
			},
		},
	}

	comps, err := NewFrameworkComponents(ctx, client, informerFactory, WithProfiles(cfg.Profiles...))
	if err != nil {
		t.Fatalf("NewFrameworkComponents failed: %v", err)
	}

	informerFactory.StartWithContext(ctx)
	res := informerFactory.WaitForCacheSyncWithContext(ctx)
	if res.Err != nil {
		t.Fatalf("WaitForCacheSyncWithContext failed: %v", res.Err)
	}

	snap := internalcache.NewEmptySnapshot()
	profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, snap)
	if err != nil {
		t.Fatalf("NewFrameworkMap failed: %v", err)
	}
	if profileMap == nil {
		t.Fatal("expected non-nil ProfileMap")
	}

	tests := []struct {
		name          string
		schedulerName string
		wantErrMsg    string
	}{
		{
			name:          "explicit default scheduler",
			schedulerName: "default-scheduler",
		},
		{
			name:          "explicit custom scheduler",
			schedulerName: "custom-scheduler",
		},
		{
			name:          "empty scheduler name defaults to default-scheduler",
			schedulerName: "",
		},
		{
			name:          "unknown scheduler name returns error",
			schedulerName: "unknown-scheduler",
			wantErrMsg:    `profile not found for scheduler name "unknown-scheduler"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: v1.PodSpec{
					SchedulerName: tt.schedulerName,
				},
			}
			f, err := profileMap.FrameworkForPod(pod)
			if err := verifyErr(tt.wantErrMsg, err); err != nil {
				t.Fatal(err)
			}
			if tt.wantErrMsg == "" && f == nil {
				t.Fatal("expected non-nil framework")
			}
		})
	}

	t.Run("FrameworkForPodGroup", func(t *testing.T) {
		t.Run("valid pod in group", func(t *testing.T) {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "group-pod",
					Namespace: "default",
				},
				Spec: v1.PodSpec{
					SchedulerName: "default-scheduler",
				},
			}
			pgInfo := &framework.PodGroupInfo{
				PodGroup:        &schedulingv1beta1.PodGroup{},
				UnscheduledPods: []*v1.Pod{pod},
			}
			f, err := profileMap.FrameworkForPodGroup(pgInfo)
			if err != nil {
				t.Fatalf("FrameworkForPodGroup failed: %v", err)
			}
			if f == nil {
				t.Fatal("expected non-nil framework")
			}
		})
		t.Run("empty pod group returns error", func(t *testing.T) {
			pgInfo := &framework.PodGroupInfo{
				PodGroup: &schedulingv1beta1.PodGroup{},
			}
			_, err := profileMap.FrameworkForPodGroup(pgInfo)
			if err == nil {
				t.Fatal("expected error for empty pod group, got nil")
			}
		})
	})
}

func TestFrameworkComponents_WithExtenders(t *testing.T) {
	ctx := t.Context()
	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	extenders := []schedulerapi.Extender{
		{
			URLPrefix:  "http://127.0.0.1:12345",
			FilterVerb: "filter",
		},
	}

	comps, err := NewFrameworkComponents(ctx, client, informerFactory, WithExtenders(extenders...))
	if err != nil {
		t.Fatalf("NewFrameworkComponents failed: %v", err)
	}
	if len(comps.extenders) != 1 {
		t.Fatalf("expected 1 extender, got %d", len(comps.extenders))
	}
}
