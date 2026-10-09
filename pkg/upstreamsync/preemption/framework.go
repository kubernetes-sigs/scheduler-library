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

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PreemptionFramework wraps framework.Framework to run PodGroup preemption.
type PreemptionFramework struct {
	framework.Framework
	PreemptionManager PreemptionManager
}

// NewPreemptionFramework creates a new PreemptionFramework.
func NewPreemptionFramework(fw framework.Framework, preemptionManager PreemptionManager) *PreemptionFramework {
	if preemptionManager == nil {
		preemptionManager = &NoopPreemptionManager{}
	}
	return &PreemptionFramework{
		Framework:         fw,
		PreemptionManager: preemptionManager,
	}
}

// SetPreemptionManager sets the PreemptionManager on the framework.
func (f *PreemptionFramework) SetPreemptionManager(pm PreemptionManager) {
	if pm == nil {
		pm = &NoopPreemptionManager{}
	}
	f.PreemptionManager = pm
}

// RunPodGroupPostFilterPlugins runs DefaultPreemption for the pod group.
// UPSTREAM-DIFF: Runs only DefaultPreemption.
func (f *PreemptionFramework) RunPodGroupPostFilterPlugins(ctx context.Context, state *framework.CycleState, podGroupInfo fwk.PodGroupInfo, podGroupSchedulingFunc fwk.PodGroupSchedulingFunc) (postFilterResult *fwk.PodGroupPostFilterResult, status *fwk.Status) {
	pm := f.PreemptionManager
	if pm == nil {
		pm = &NoopPreemptionManager{}
	}
	defaultPreemption := NewDefaultPreemption(f.Framework, pm)
	return defaultPreemption.PodGroupPostFilter(ctx, state, podGroupInfo, podGroupSchedulingFunc)
}
