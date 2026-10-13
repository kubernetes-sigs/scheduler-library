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

// Package preemption ports the kube-scheduler PodGroup preemption evaluator and
// PreemptionManager interfaces from Kubernetes 1.38.
//
// UPSTREAM-DIFF: This package exists because go.mod pins k8s.io/kubernetes to v1.37.0,
// whereas the PreemptionManager interface (kubernetes/kubernetes#141932) and
// ReprieveFilter (kubernetes/kubernetes#142393) landed in Kubernetes 1.38.
//
// Migration Plan:
// Once go.mod updates to Kubernetes 1.38 (with SetPreemptionManager(pm fwk.PreemptionManager)
// added to framework.Framework in k8s.io/kubernetes/pkg/scheduler/framework):
//  1. Delete this entire package (pkg/upstreamsync/preemption).
//  2. Delete wrapRegistryForPreemption and preemptionFrameworks from ProfileMap in pkg/upstreamsync/framework_init.go.
//  3. Remove the preemption.Handle type assertion in out-of-tree PodGroupPostFilter plugins and use fwk.Handle directly.
package preemption

import (
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Handle extends fwk.Handle with PreemptionManager() to mirror the Kubernetes 1.38
// fwk.Handle interface on v1.37.0.
//
// UPSTREAM-DIFF: Can be removed once go.mod updates to Kubernetes 1.38
// (kubernetes/kubernetes#141932).
type Handle interface {
	fwk.Handle
	// PreemptionManager returns the preemption manager associated with the framework.
	// Because simulation harnesses replace the PreemptionManager between scheduling cycles
	// via SetPreemptionManager, plugins must call PreemptionManager() during PostFilter or
	// PodGroupPostFilter execution and must not cache the returned value during plugin initialization.
	PreemptionManager() PreemptionManager
}

// PreemptionFramework wraps framework.Framework to expose Handle (including
// PreemptionManager) and SetPreemptionManager on Kubernetes v1.37.0.
//
// UPSTREAM-DIFF: Can be removed once go.mod updates to Kubernetes 1.38 and
// SetPreemptionManager is added to framework.Framework.
type PreemptionFramework struct {
	framework.Framework
	pm PreemptionManager
}

var _ Handle = (*PreemptionFramework)(nil)

// NewPreemptionFramework creates a new PreemptionFramework.
func NewPreemptionFramework(fw framework.Framework, preemptionManager PreemptionManager) *PreemptionFramework {
	if preemptionManager == nil {
		preemptionManager = &NoopPreemptionManager{}
	}
	return &PreemptionFramework{
		Framework: fw,
		pm:        preemptionManager,
	}
}

// PreemptionManager returns the active PreemptionManager for the framework.
func (f *PreemptionFramework) PreemptionManager() PreemptionManager {
	if f.pm == nil {
		return &NoopPreemptionManager{}
	}
	return f.pm
}

// SetPreemptionManager sets the PreemptionManager on the framework.
func (f *PreemptionFramework) SetPreemptionManager(pm PreemptionManager) {
	if pm == nil {
		pm = &NoopPreemptionManager{}
	}
	f.pm = pm
}
