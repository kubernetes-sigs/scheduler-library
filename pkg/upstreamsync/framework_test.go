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

package upstreamsync_test

import (
	"context"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	libframework "sigs.k8s.io/scheduler-library/pkg/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

const testPluginName = "OutOfTreeTestPlugin"

// recordedPlugin is a minimal out-of-tree plugin that remembers what the framework handed
// to its factory, so tests can assert on it.
type recordedPlugin struct {
	name   string
	args   runtime.Object
	handle fwk.Handle
}

var _ fwk.FilterPlugin = &recordedPlugin{}

func (p *recordedPlugin) Name() string { return p.name }

func (p *recordedPlugin) Filter(_ context.Context, _ fwk.CycleState, _ *v1.Pod, _ fwk.NodeInfo) *fwk.Status {
	return nil
}

// factoryRecorder counts factory invocations and keeps every plugin instance it built.
type factoryRecorder struct {
	calls   int
	plugins []*recordedPlugin
}

func (r *factoryRecorder) factory(name string) frameworkruntime.PluginFactory {
	return func(_ context.Context, args runtime.Object, h fwk.Handle) (fwk.Plugin, error) {
		p := &recordedPlugin{name: name, args: args, handle: h}
		r.calls++
		r.plugins = append(r.plugins, p)
		return p, nil
	}
}

type discardRecorder struct{}

var _ events.EventRecorderLogger = discardRecorder{}

func (discardRecorder) Eventf(_ runtime.Object, _ runtime.Object, _, _, _, _ string, _ ...interface{}) {
}

func (d discardRecorder) WithLogger(_ klog.Logger) events.EventRecorderLogger { return d }

// testProfile builds a minimal valid profile with the given plugins enabled on Filter.
func testProfile(filterPlugins ...string) schedulerapi.KubeSchedulerProfile {
	filter := schedulerapi.PluginSet{}
	for _, name := range filterPlugins {
		filter.Enabled = append(filter.Enabled, schedulerapi.Plugin{Name: name})
	}
	return schedulerapi.KubeSchedulerProfile{
		SchedulerName: v1.DefaultSchedulerName,
		Plugins: &schedulerapi.Plugins{
			QueueSort: schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "PrioritySort"}}},
			Bind:      schedulerapi.PluginSet{Enabled: []schedulerapi.Plugin{{Name: "DefaultBinder"}}},
			Filter:    filter,
		},
	}
}

func newProfileMap(t *testing.T, prof schedulerapi.KubeSchedulerProfile, opts ...upstreamsync.Option) (*upstreamsync.ProfileMap, error) {
	t.Helper()
	libframework.InitMetricsOnce()

	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)
	recorderFactory := func(string) events.EventRecorderLogger { return discardRecorder{} }

	allOpts := append([]upstreamsync.Option{upstreamsync.WithProfiles(prof)}, opts...)
	return upstreamsync.NewProfileMap(
		t.Context(),
		client,
		informerFactory,
		recorderFactory,
		nil, // podNominator
		nil, // podActivator
		nil, // apiCacher
		internalcache.NewEmptySnapshot(),
		allOpts...,
	)
}

func TestWithFrameworkOutOfTreeRegistry(t *testing.T) {
	rec := &factoryRecorder{}
	profileMap, err := newProfileMap(t, testProfile(testPluginName),
		upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
			testPluginName: rec.factory(testPluginName),
		}),
	)
	if err != nil {
		t.Fatalf("NewProfileMap failed: %v", err)
	}

	if rec.calls != 1 {
		t.Errorf("Expected the out-of-tree factory to be called once, got %d", rec.calls)
	}

	f, ok := profileMap.Map[v1.DefaultSchedulerName]
	if !ok {
		t.Fatalf("Profile %q not found", v1.DefaultSchedulerName)
	}
	var found bool
	for _, p := range f.ListPlugins().Filter.Enabled {
		if p.Name == testPluginName {
			found = true
		}
	}
	if !found {
		t.Errorf("Expected %q on the Filter extension point, got %v", testPluginName, f.ListPlugins().Filter.Enabled)
	}
}

func TestWithFrameworkOutOfTreeRegistryNameCollision(t *testing.T) {
	rec := &factoryRecorder{}
	_, err := newProfileMap(t, testProfile("NodeResourcesFit"),
		upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
			"NodeResourcesFit": rec.factory("NodeResourcesFit"),
		}),
	)
	if err == nil {
		t.Fatal("Expected an error when an out-of-tree plugin shadows an in-tree one, got nil")
	}
	for _, want := range []string{"merging out-of-tree plugin registry", "a plugin named NodeResourcesFit already exists"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Expected error to contain %q, got: %v", want, err)
		}
	}
}

func TestOutOfTreePluginMissingFromRegistry(t *testing.T) {
	_, err := newProfileMap(t, testProfile(testPluginName))
	if err == nil {
		t.Fatal("Expected an error when the profile enables an unregistered plugin, got nil")
	}
	if !strings.Contains(err.Error(), testPluginName) {
		t.Errorf("Expected error to mention %q, got: %v", testPluginName, err)
	}
}

func TestOutOfTreePluginArgs(t *testing.T) {
	typedArgs := &schedulerapi.DefaultPreemptionArgs{MinCandidateNodesPercentage: 42}
	unknownArgs := &runtime.Unknown{Raw: []byte(`{"threshold":7}`)}

	tests := []struct {
		name   string
		args   runtime.Object
		verify func(t *testing.T, got runtime.Object)
	}{
		{
			name: "typed runtime.Object is passed through unchanged",
			args: typedArgs,
			verify: func(t *testing.T, got runtime.Object) {
				if got != runtime.Object(typedArgs) {
					t.Errorf("Expected the factory to receive the configured args %#v, got %#v", typedArgs, got)
				}
			},
		},
		{
			name: "runtime.Unknown can be decoded by the plugin",
			args: unknownArgs,
			verify: func(t *testing.T, got runtime.Object) {
				var decoded struct {
					Threshold int `json:"threshold"`
				}
				if err := frameworkruntime.DecodeInto(got, &decoded); err != nil {
					t.Fatalf("DecodeInto failed: %v", err)
				}
				if decoded.Threshold != 7 {
					t.Errorf("Expected threshold 7, got %d", decoded.Threshold)
				}
			},
		},
		{
			name: "no plugin config yields nil args",
			args: nil,
			verify: func(t *testing.T, got runtime.Object) {
				if got != nil {
					t.Errorf("Expected nil args, got %#v", got)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prof := testProfile(testPluginName)
			if tc.args != nil {
				prof.PluginConfig = []schedulerapi.PluginConfig{{Name: testPluginName, Args: tc.args}}
			}

			rec := &factoryRecorder{}
			if _, err := newProfileMap(t, prof,
				upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					testPluginName: rec.factory(testPluginName),
				}),
			); err != nil {
				t.Fatalf("NewProfileMap failed: %v", err)
			}
			if len(rec.plugins) != 1 {
				t.Fatalf("Expected 1 plugin instance, got %d", len(rec.plugins))
			}
			tc.verify(t, rec.plugins[0].args)
		})
	}
}

func TestOutOfTreePluginNotEnabledIsNotInstantiated(t *testing.T) {
	rec := &factoryRecorder{}
	// The registry carries the plugin, but no extension point of the profile enables it.
	if _, err := newProfileMap(t, testProfile(),
		upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
			testPluginName: rec.factory(testPluginName),
		}),
	); err != nil {
		t.Fatalf("NewProfileMap failed: %v", err)
	}
	if rec.calls != 0 {
		t.Errorf("Expected the factory of a disabled plugin not to be called, got %d calls", rec.calls)
	}
}

func TestWithKubeConfig(t *testing.T) {
	cfg := &restclient.Config{Host: "https://127.0.0.1:1"}

	tests := []struct {
		name string
		opts []upstreamsync.Option
		want *restclient.Config
	}{
		{
			name: "config is exposed to plugins",
			opts: []upstreamsync.Option{upstreamsync.WithKubeConfig(cfg)},
			want: cfg,
		},
		{
			name: "no option leaves the handle config nil",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &factoryRecorder{}
			opts := append([]upstreamsync.Option{
				upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					testPluginName: rec.factory(testPluginName),
				}),
			}, tc.opts...)

			if _, err := newProfileMap(t, testProfile(testPluginName), opts...); err != nil {
				t.Fatalf("NewProfileMap failed: %v", err)
			}
			if len(rec.plugins) != 1 {
				t.Fatalf("Expected 1 plugin instance, got %d", len(rec.plugins))
			}
			if got := rec.plugins[0].handle.KubeConfig(); got != tc.want {
				t.Errorf("handle.KubeConfig() = %v, want %v", got, tc.want)
			}
		})
	}
}
