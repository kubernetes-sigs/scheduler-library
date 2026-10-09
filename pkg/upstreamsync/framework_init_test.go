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
	"context"
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	fwk "k8s.io/kube-scheduler/framework"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	internalcache "k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
)

// verifyErr verifies that the given error is the error expected by the test case.
func verifyErr(wantErrMsg string, err error) error {
	if wantErrMsg != "" && err == nil {
		return fmt.Errorf("want error, got nil")
	}

	if wantErrMsg == "" && err != nil {
		return fmt.Errorf("want no error, got %w", err)
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
	if err := comps.WaitForHandlersSync(ctx); err != nil {
		t.Fatalf("comps.WaitForHandlersSync failed: %v", err)
	}

	snap := internalcache.NewEmptySnapshot()
	profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, snap)
	if err != nil {
		t.Fatalf("NewFrameworkMap failed: %v", err)
	}
	if profileMap == nil {
		t.Fatal("expected non-nil ProfileMap")
	}
	informerFactory.StartWithContext(ctx)
	if res := informerFactory.WaitForCacheSyncWithContext(ctx); res.Err != nil {
		t.Fatalf("WaitForCacheSyncWithContext failed: %v", res.Err)
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

// stubDRAManager stands in for the informer-backed manager. The test only checks which
// manager the frameworks were given, so none of the accessors are ever called.
type stubDRAManager struct{}

var _ fwk.SharedDRAManager = &stubDRAManager{}

func (s *stubDRAManager) ResourceClaims() fwk.ResourceClaimTracker     { return nil }
func (s *stubDRAManager) ResourceSlices() fwk.ResourceSliceLister      { return nil }
func (s *stubDRAManager) DeviceClasses() fwk.DeviceClassLister         { return nil }
func (s *stubDRAManager) DeviceClassResolver() fwk.DeviceClassResolver { return nil }

func TestNewFrameworkMapSharedDRAManager(t *testing.T) {
	stub := &stubDRAManager{}

	tests := []struct {
		name     string
		opts     []FrameworkMapOption
		wantStub bool
	}{
		{
			name:     "a supplied DRA manager replaces the default one",
			opts:     []FrameworkMapOption{WithSharedDRAManager(stub)},
			wantStub: true,
		},
		{
			name: "without a supplied DRA manager the default one is built",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			client := fake.NewClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			comps, err := NewFrameworkComponents(ctx, client, informerFactory)
			if err != nil {
				t.Fatalf("NewFrameworkComponents failed: %v", err)
			}

			profileMap, err := NewFrameworkMap(ctx, comps, fakeRecorderFactory, internalcache.NewEmptySnapshot(), tt.opts...)
			if err != nil {
				t.Fatalf("NewFrameworkMap failed: %v", err)
			}

			got := profileMap.Map[v1.DefaultSchedulerName].SharedDRAManager()
			switch {
			case tt.wantStub && got != stub:
				t.Errorf("SharedDRAManager() = %v, want the supplied stub", got)
			case !tt.wantStub && got == nil:
				t.Error("SharedDRAManager() = nil, want the default manager")
			case !tt.wantStub && got == stub:
				t.Error("SharedDRAManager() returned the stub, want the default manager")
			}
		})
	}
}

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

func newProfileMap(t *testing.T, prof schedulerapi.KubeSchedulerProfile, opts ...Option) (*ProfileMap, error) {
	t.Helper()
	ctx := t.Context()

	client := fake.NewClientset()
	informerFactory := informers.NewSharedInformerFactory(client, 0)

	allOpts := append([]Option{WithProfiles(prof)}, opts...)
	comps, err := NewFrameworkComponents(ctx, client, informerFactory, allOpts...)
	if err != nil {
		return nil, err
	}
	return NewFrameworkMap(
		ctx,
		comps,
		fakeRecorderFactory,
		internalcache.NewEmptySnapshot(),
	)
}

func TestWithFrameworkOutOfTreeRegistryCloned(t *testing.T) {
	const pluginName = "CustomPlugin"
	reg := frameworkruntime.Registry{
		pluginName: nil,
	}
	opt := WithFrameworkOutOfTreeRegistry(reg)
	delete(reg, pluginName)

	var opts schedulerOptions
	opt(&opts)

	if _, ok := opts.frameworkOutOfTreeRegistry[pluginName]; !ok {
		t.Errorf("expected %q to remain in frameworkOutOfTreeRegistry after mutating caller map", pluginName)
	}
}

func TestWithFrameworkOutOfTreeRegistry(t *testing.T) {
	tests := []struct {
		name             string
		prof             schedulerapi.KubeSchedulerProfile
		registryPlugin   string
		wantErr          bool
		wantCalls        int
		wantFilterPlugin string
	}{
		{
			name:             "out-of-tree plugin enabled in profile is instantiated and registered",
			prof:             testProfile(testPluginName),
			registryPlugin:   testPluginName,
			wantCalls:        1,
			wantFilterPlugin: testPluginName,
		},
		{
			name:           "out-of-tree plugin name collision with in-tree plugin returns error",
			prof:           testProfile("NodeResourcesFit"),
			registryPlugin: "NodeResourcesFit",
			wantErr:        true,
		},
		{
			name:    "unregistered plugin enabled in profile returns error",
			prof:    testProfile(testPluginName),
			wantErr: true,
		},
		{
			name:           "out-of-tree plugin not enabled in profile is not instantiated",
			prof:           testProfile(),
			registryPlugin: testPluginName,
			wantCalls:      0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := &factoryRecorder{}
			var opts []Option
			if tc.registryPlugin != "" {
				opts = append(opts, WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					tc.registryPlugin: rec.factory(tc.registryPlugin),
				}))
			}

			profileMap, err := newProfileMap(t, tc.prof, opts...)
			if (err != nil) != tc.wantErr {
				t.Fatalf("newProfileMap() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}

			if rec.calls != tc.wantCalls {
				t.Errorf("Expected the out-of-tree factory to be called %d times, got %d", tc.wantCalls, rec.calls)
			}

			if tc.wantFilterPlugin != "" {
				f, ok := profileMap.Map[v1.DefaultSchedulerName]
				if !ok {
					t.Fatalf("Profile %q not found", v1.DefaultSchedulerName)
				}
				var found bool
				for _, p := range f.ListPlugins().Filter.Enabled {
					if p.Name == tc.wantFilterPlugin {
						found = true
					}
				}
				if !found {
					t.Errorf("Expected %q on the Filter extension point, got %v", tc.wantFilterPlugin, f.ListPlugins().Filter.Enabled)
				}
			}
		})
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
				WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					testPluginName: rec.factory(testPluginName),
				}),
			); err != nil {
				t.Fatalf("newProfileMap failed: %v", err)
			}
			if len(rec.plugins) != 1 {
				t.Fatalf("Expected 1 plugin instance, got %d", len(rec.plugins))
			}
			tc.verify(t, rec.plugins[0].args)
		})
	}
}

func TestWithKubeConfig(t *testing.T) {
	cfg := &restclient.Config{Host: "https://127.0.0.1:1"}

	tests := []struct {
		name string
		opts []Option
		want *restclient.Config
	}{
		{
			name: "config is exposed to plugins",
			opts: []Option{WithKubeConfig(cfg)},
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
			opts := append([]Option{
				WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
					testPluginName: rec.factory(testPluginName),
				}),
			}, tc.opts...)

			if _, err := newProfileMap(t, testProfile(testPluginName), opts...); err != nil {
				t.Fatalf("newProfileMap failed: %v", err)
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
