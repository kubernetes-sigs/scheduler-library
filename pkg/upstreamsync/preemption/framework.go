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

package preemption

import (
	"context"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

type preemptionManagerKey struct{}

// WithPreemptionManager returns a copy of ctx that carries pm.
func WithPreemptionManager(ctx context.Context, pm PreemptionManager) context.Context {
	return context.WithValue(ctx, preemptionManagerKey{}, pm)
}

// ManagerFromContext extracts the PreemptionManager from ctx.
// If ctx does not carry a PreemptionManager, ManagerFromContext returns a NoopPreemptionManager.
func ManagerFromContext(ctx context.Context) PreemptionManager {
	if ctx != nil {
		if pm, ok := ctx.Value(preemptionManagerKey{}).(PreemptionManager); ok && pm != nil {
			return pm
		}
	}
	return &NoopPreemptionManager{}
}

// contextPreemptionManager is a stateless PreemptionManager and PreemptionExecutor
// that delegates context-carrying operations to the PreemptionManager in context.Context.
type contextPreemptionManager struct {
	NoopPreemptionManager
}

var _ PreemptionManager = &contextPreemptionManager{}
var _ PreemptionExecutor = &contextPreemptionManager{}

// NewContextPreemptionManager returns a stateless PreemptionManager that resolves
// the active PreemptionManager from context.Context on each operation.
func NewContextPreemptionManager() PreemptionManager {
	return &contextPreemptionManager{}
}

func (c *contextPreemptionManager) GenerateVictims(ctx context.Context, pgInfo fwk.PodGroupInfo) ([]PreemptionVictim, *fwk.Status) {
	return ManagerFromContext(ctx).GenerateVictims(ctx, pgInfo)
}

func (c *contextPreemptionManager) Executor() PreemptionExecutor {
	return c
}

func (c *contextPreemptionManager) NewReprieveFilter(ctx context.Context, allVictims []PreemptionVictim) ReprieveFilter {
	return ManagerFromContext(ctx).NewReprieveFilter(ctx, allVictims)
}

func (c *contextPreemptionManager) ActuatePodPreemption(ctx context.Context, candidate PreemptionCandidate, pod *v1.Pod, pluginName string) *fwk.Status {
	return ManagerFromContext(ctx).Executor().ActuatePodPreemption(ctx, candidate, pod, pluginName)
}

func (c *contextPreemptionManager) ActuatePodGroupPreemption(ctx context.Context, candidate PreemptionCandidate, pgInfo fwk.PodGroupInfo, pluginName string) *fwk.Status {
	return ManagerFromContext(ctx).Executor().ActuatePodGroupPreemption(ctx, candidate, pgInfo, pluginName)
}

// PreemptionFramework wraps framework.Framework to run PodGroup preemption.
type PreemptionFramework struct {
	framework.Framework
	PreemptionManager PreemptionManager
}

// NewPreemptionFramework creates a new PreemptionFramework.
func NewPreemptionFramework(fw framework.Framework, preemptionManager PreemptionManager) *PreemptionFramework {
	if preemptionManager == nil {
		preemptionManager = NewContextPreemptionManager()
	}
	return &PreemptionFramework{
		Framework:         fw,
		PreemptionManager: preemptionManager,
	}
}

// RunPodGroupPostFilterPlugins runs DefaultPreemption for the pod group.
// UPSTREAM-DIFF: Runs only DefaultPreemption.
func (f *PreemptionFramework) RunPodGroupPostFilterPlugins(ctx context.Context, state *framework.CycleState, podGroupInfo fwk.PodGroupInfo, podGroupSchedulingFunc fwk.PodGroupSchedulingFunc) (postFilterResult *fwk.PodGroupPostFilterResult, status *fwk.Status) {
	pm := f.PreemptionManager
	if pm == nil {
		pm = NewContextPreemptionManager()
	}
	defaultPreemption := NewDefaultPreemption(f.Framework, pm)
	postFilterResult, status = defaultPreemption.PodGroupPostFilter(ctx, state, podGroupInfo, podGroupSchedulingFunc)
	return postFilterResult, status
}
