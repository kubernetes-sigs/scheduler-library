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

// PreemptionManagerFactory is a factory for creating PreemptionManager instances.
type PreemptionManagerFactory func() PreemptionManager

// PreemptionFramework is a wrapper around the kube-scheduler framework that adds preemption logic.
type PreemptionFramework struct {
	framework.Framework
	PreemptionManager PreemptionManager
}

// NewPreemptionFramework creates a new PreemptionFramework.
func NewPreemptionFramework(fw framework.Framework, preemptionManager PreemptionManager) *PreemptionFramework {
	return &PreemptionFramework{
		Framework:         fw,
		PreemptionManager: preemptionManager,
	}
}

// RunPodGroupPostFilterPlugins is implementation of framework.Framework.RunPodGroupPostFilterPlugins.
// UPSTREAM-DIFF: It runs only one plugin - defaultPreemption.
func (f *PreemptionFramework) RunPodGroupPostFilterPlugins(ctx context.Context, state *framework.CycleState, podGroupInfo fwk.PodGroupInfo, podGroupSchedulingFunc fwk.PodGroupSchedulingFunc) (postFilterResult *fwk.PodGroupPostFilterResult, status *fwk.Status) {
	defaultPreemption := NewDefaultPreemption(f.Framework, f.PreemptionManager)
	postFilterResult, status = defaultPreemption.PodGroupPostFilter(ctx, state, podGroupInfo, podGroupSchedulingFunc)
	return postFilterResult, status
}

// NoopPreemptionManagerFactory is a PreemptionManagerFactory that return dummy PreemptionManager.
func NoopPreemptionManagerFactory() PreemptionManager {
	return &NoopPreemptionManager{}
}
