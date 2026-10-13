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

// Package preemption implements workload preemption simulation adapters and
// prefix-search evaluation for scheduler-library.
package preemption

import (
	"context"
	"fmt"
	"iter"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	upstreampreemption "sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// PreemptionVictim is an atomic unit of preemption, which can contain 1 or more pods.
type PreemptionVictim interface {
	// Pods can contain 1 or more pods.
	// The returned pods must not overlap across victims and must be present in the
	// snapshot with Spec.NodeName set.
	Pods() []*v1.Pod
}

// PreemptionFilter provides a fast pre-check to skip expensive scheduling evaluations
// when the current set of preempted victims is known to be insufficient.
type PreemptionFilter interface {
	// UnpreemptVictim is called whenever a victim has been removed from the current victim set.
	UnpreemptVictim(PreemptionVictim)

	// PreemptVictim is called whenever a victim has been added to the current victim set.
	PreemptVictim(PreemptionVictim)

	// MayFit reports whether the preemptor could potentially fit given
	// the current victim set.
	//
	// If it returns true, the scheduler runs a scheduling evaluation for the preemptor.
	// If it returns false, the current victim set is treated as insufficient and
	// the scheduling evaluation is skipped.
	//
	// Implementations must rely only on internal state updated via
	// [PreemptionFilter.PreemptVictim] and [PreemptionFilter.UnpreemptVictim],
	// and must not read from the cluster snapshot directly.
	//
	// The result must be monotonic with respect to the victim set:
	//   - [PreemptionFilter.PreemptVictim] must never change the result from true to false.
	//   - [PreemptionFilter.UnpreemptVictim] must never change the result from false to true.
	MayFit() bool
}

// WorkloadPreemptionOptions configures victim selection and filtering for workload preemption.
//
// When a workload cannot be scheduled directly, preemption runs in two stages:
//  1. Prefix search: victims from [WorkloadPreemptionOptions.PotentialVictims] are preempted
//     in order (least to most valuable) until the workload fits.
//  2. Reprieval: victims in that prefix are tested in reverse order (most to least valuable)
//     and restored to the cluster if the workload still fits without preempting them.
type WorkloadPreemptionOptions struct {
	// PotentialVictims yields candidate victims that may be preempted to make room for the
	// workload, ordered from least to most valuable.
	// It is consumed during stage 1 to find the smallest prefix of victims needed for the
	// workload to fit, after which stage 2 attempts to reprieve victims from that prefix.
	// If nil or empty, no additional victims are considered during stage 1.
	PotentialVictims iter.Seq[PreemptionVictim]

	// CommittedVictims are victims that are preempted after the unsuccessful initial scheduling attempt
	// but before stage 1 begins.
	// Unlike PotentialVictims, they are never candidates for reprieval in stage 2.
	// Each committed victim is passed to [PreemptionFilter.PreemptVictim] before stage 1.
	// If nil or empty, no victims are preempted upfront.
	CommittedVictims []PreemptionVictim

	// PreemptionFilter is an optional fast pre-check used during both prefix search and
	// reprieval to skip scheduling evaluations when the current victim set is known
	// to be insufficient.
	// If nil, no pre-check filter is applied.
	PreemptionFilter PreemptionFilter
}

// simPreemptionVictim adapts a PreemptionVictim to upstreampreemption.PreemptionVictim.
type simPreemptionVictim struct {
	victim    PreemptionVictim
	pods      []fwk.PodInfo
	reprieved bool
}

var _ upstreampreemption.PreemptionVictim = (*simPreemptionVictim)(nil)

func newSimPreemptionVictim(v PreemptionVictim) (*simPreemptionVictim, error) {
	if v == nil {
		return nil, fmt.Errorf("preemption victim cannot be nil")
	}
	rawPods := v.Pods()
	podInfos := make([]fwk.PodInfo, 0, len(rawPods))
	for _, p := range rawPods {
		if p == nil {
			return nil, fmt.Errorf("preemption victim pod cannot be nil")
		}
		pi, err := framework.NewPodInfo(p)
		if err != nil {
			return nil, fmt.Errorf("failed to create PodInfo for victim pod: %w", err)
		}
		podInfos = append(podInfos, pi)
	}
	return &simPreemptionVictim{
		victim: v,
		pods:   podInfos,
	}, nil
}

func (v *simPreemptionVictim) Pods() []fwk.PodInfo {
	return v.pods
}

func (v *simPreemptionVictim) NumPDBViolations() int {
	return 0
}

// prefixPreemptionManager provides candidate victims and creates a ReprieveFilter.
type prefixPreemptionManager struct {
	upstreampreemption.NoopPreemptionManager
	victims          []*simPreemptionVictim
	preemptionFilter PreemptionFilter
	actuated         bool
}

var _ upstreampreemption.PreemptionManager = (*prefixPreemptionManager)(nil)
var _ upstreampreemption.PreemptionExecutor = (*prefixPreemptionManager)(nil)

func newPrefixPreemptionManager(filter PreemptionFilter) *prefixPreemptionManager {
	return &prefixPreemptionManager{
		preemptionFilter: filter,
	}
}

func (pm *prefixPreemptionManager) addVictim(sv *simPreemptionVictim) {
	pm.victims = append(pm.victims, sv)
}

func (pm *prefixPreemptionManager) Executor() upstreampreemption.PreemptionExecutor {
	return pm
}

func (pm *prefixPreemptionManager) ActuatePodGroupPreemption(_ context.Context, _ upstreampreemption.PreemptionCandidate, _ fwk.PodGroupInfo, _ string) *fwk.Status {
	pm.actuated = true
	return nil
}

func (pm *prefixPreemptionManager) GenerateVictims(
	_ context.Context,
	_ fwk.PodGroupInfo,
) ([]upstreampreemption.PreemptionVictim, *fwk.Status) {
	res := make([]upstreampreemption.PreemptionVictim, len(pm.victims))
	for i, sv := range pm.victims {
		res[i] = sv
	}
	return res, fwk.NewStatus(fwk.Success)
}

func (pm *prefixPreemptionManager) NewReprieveFilter(
	_ context.Context,
	_ []upstreampreemption.PreemptionVictim,
) upstreampreemption.ReprieveFilter {
	return &workloadReprieveFilter{filter: pm.preemptionFilter}
}

func (pm *prefixPreemptionManager) preemptedVictims() []PreemptionVictim {
	var res []PreemptionVictim
	for _, sv := range pm.victims {
		if !sv.reprieved {
			res = append(res, sv.victim)
		}
	}
	return res
}

// workloadReprieveFilter evaluates and tracks victim reprieval using PreemptionFilter.
type workloadReprieveFilter struct {
	filter PreemptionFilter
}

var _ upstreampreemption.ReprieveFilter = (*workloadReprieveFilter)(nil)

func (rf *workloadReprieveFilter) ShouldAttemptReprieval(
	_ context.Context,
	victim upstreampreemption.PreemptionVictim,
) (bool, error) {
	if rf.filter == nil {
		return true, nil
	}
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return false, fmt.Errorf("unexpected PreemptionVictim type %T", victim)
	}
	rf.filter.UnpreemptVictim(sv.victim)
	mayFit := rf.filter.MayFit()
	rf.filter.PreemptVictim(sv.victim)
	return mayFit, nil
}

func (rf *workloadReprieveFilter) OnVictimReprieved(
	_ context.Context,
	victim upstreampreemption.PreemptionVictim,
) error {
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return fmt.Errorf("unexpected PreemptionVictim type %T", victim)
	}
	sv.reprieved = true
	if rf.filter != nil {
		rf.filter.UnpreemptVictim(sv.victim)
	}
	return nil
}

// EvaluateWorkloadPreemption runs Stage 1b (prefix search over PotentialVictims)
// and Stage 2 (victim reprieval via PodGroupPostFilter plugins).
// When preemption succeeds, it returns the proposed pod assignments, the non-reprieved
// victims from the prefix, and a nil error status.
// When preemption fails without error, it returns (nil, nil, nil).
func EvaluateWorkloadPreemption(
	ctx context.Context,
	schedFramework *upstreampreemption.PreemptionFramework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
	baseSchedulingFunc fwk.PodGroupSchedulingFunc,
	opts WorkloadPreemptionOptions,
) ([]fwk.ProposedAssignment, []PreemptionVictim, *fwk.Status) {
	if opts.PotentialVictims == nil {
		return nil, nil, nil
	}

	var lastAssignments *fwk.PodGroupAssignments
	pgSchedulingFunc := func(ctx context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
		assignments, status := baseSchedulingFunc(ctx)
		if status.IsSuccess() {
			lastAssignments = assignments
		}
		return assignments, status
	}

	pm := newPrefixPreemptionManager(opts.PreemptionFilter)
	schedFramework.SetPreemptionManager(pm)
	defer schedFramework.SetPreemptionManager(&upstreampreemption.NoopPreemptionManager{})

	for v := range opts.PotentialVictims {
		if v == nil || len(v.Pods()) == 0 {
			continue
		}
		sv, err := newSimPreemptionVictim(v)
		if err != nil {
			return nil, nil, fwk.AsStatus(err)
		}
		pm.addVictim(sv)

		if opts.PreemptionFilter != nil {
			opts.PreemptionFilter.PreemptVictim(v)
			if !opts.PreemptionFilter.MayFit() {
				continue
			}
		}

		_, postFilterStatus := schedFramework.RunPodGroupPostFilterPlugins(ctx, podGroupCycleState, podGroupInfo, pgSchedulingFunc)
		if postFilterStatus.IsError() {
			return nil, nil, postFilterStatus
		}
		if postFilterStatus.IsSuccess() && pm.actuated && lastAssignments != nil {
			return lastAssignments.ProposedAssignments, pm.preemptedVictims(), nil
		}
	}

	return nil, nil, nil
}
