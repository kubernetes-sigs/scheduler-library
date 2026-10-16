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

// Package preemption provides workload preemption types and simulation adapters
// that bridge the library's PreemptionVictim and PreemptionFilter interfaces to
// the scheduler framework's PreemptionManager and ReprieveFilter interfaces.
package preemption

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	upstreampreemption "sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// EvaluateWorkloadPreemption finds the minimal prefix of victims from opts.PotentialVictims
// that allows the workload to fit and then reprieves unneeded victims in reverse order.
// If opts.PreemptionFilter is set, it first fast-forwards through candidate victims until
// MayFit returns true. Once MayFit holds, it uses exponential and binary search (with reprieval
// disabled) to locate the minimal fitting prefix, followed by a final reprieval pass on the
// minimal winning prefix.
// It returns the last successful PodGroupAssignments along with the victims that were not reprieved.
func EvaluateWorkloadPreemption(
	ctx context.Context,
	schedFramework *upstreampreemption.PreemptionFramework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo fwk.PodGroupInfo,
	baseSchedulingFunc fwk.PodGroupSchedulingFunc,
	opts WorkloadPreemptionOptions,
) (*fwk.PodGroupAssignments, []PreemptionVictim, *fwk.Status) {
	if opts.PotentialVictims == nil {
		return nil, nil, fwk.NewStatus(fwk.Unschedulable)
	}

	pm := newPrefixPreemptionManager(opts.PreemptionFilter)
	schedFramework.SetPreemptionManager(pm)
	defer schedFramework.SetPreemptionManager(&NoopPreemptionManager{})

	var lastAssignments *fwk.PodGroupAssignments
	pgSchedulingFunc := func(ctx context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
		assignments, status := baseSchedulingFunc(ctx)
		if status.IsSuccess() {
			lastAssignments = assignments
		}
		return assignments, status
	}

	probe := func(allowReprieve bool) (bool, *fwk.Status) {
		pm.allowReprieve = allowReprieve
		_, status := schedFramework.RunPodGroupPostFilterPlugins(ctx, podGroupCycleState, podGroupInfo, pgSchedulingFunc)
		if status.IsError() {
			return false, status
		}
		return status.IsSuccess(), nil
	}

	// Step 2a: Quota Fast-Forward & Exponential Search over opts.PotentialVictims.
	// base tracks the largest prefix where PreemptionFilter.MayFit() was false.
	// Once MayFit() holds (quotaSatisfied = true), monotonicity guarantees MayFit()
	// stays true for all prefixes > base, and we probe at base + 1, base + 2, base + 4, base + 8, ...
	base := 0
	lo := 0
	offset := 1
	found := false
	quotaSatisfied := opts.PreemptionFilter == nil

	for v := range opts.PotentialVictims {
		if v == nil || len(v.Pods()) == 0 {
			continue
		}
		sv, err := newSimPreemptionVictim(v)
		if err != nil {
			return nil, nil, fwk.AsStatus(err)
		}
		pm.addVictim(sv)

		if !quotaSatisfied {
			pm.syncFilter(pm.cursor)
			if !opts.PreemptionFilter.MayFit() {
				base = pm.cursor
				lo = pm.cursor
				continue
			}
			quotaSatisfied = true
		}

		if pm.cursor != base+offset {
			continue
		}

		fits, errStatus := probe(false)
		if errStatus != nil {
			return nil, nil, errStatus
		}
		if fits {
			found = true
			break
		}
		lo = pm.cursor
		offset *= 2
	}

	// If the iterator ended mid-step with unprobed victims above lo, probe the full slice.
	if !found {
		if pm.cursor <= lo {
			return nil, nil, fwk.NewStatus(fwk.Unschedulable)
		}
		fits, errStatus := probe(false)
		if errStatus != nil {
			return nil, nil, errStatus
		}
		if !fits {
			return nil, nil, fwk.NewStatus(fwk.Unschedulable)
		}
	}

	// Step 2b: Binary Search over (lo, hi] with allowReprieve = false to locate minimal k* = hi.
	hi := pm.cursor
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		pm.cursor = mid
		fits, errStatus := probe(false)
		if errStatus != nil {
			return nil, nil, errStatus
		}
		if fits {
			hi = mid
		} else {
			lo = mid
		}
	}

	// Step 3: Final Reprieval pass on minimal prefix V[0:hi] with allowReprieve = true.
	// Note: Step 3 intentionally re-runs RunPodGroupPostFilterPlugins (and thus podGroupSchedulingFunc)
	// on V[0:hi] to respect upstream PodGroupEvaluator.Preempt's StartMutations/EndMutations lifecycle.
	// Avoiding the second podGroupSchedulingFunc call at hi by reusing the placement from Step 2
	// is a potential future optimization if profiling ever indicates a bottleneck.
	pm.cursor = hi
	pm.syncFilter(hi)
	fits, errStatus := probe(true)
	if errStatus != nil {
		return nil, nil, errStatus
	}
	if !fits {
		return nil, nil, fwk.NewStatus(fwk.Unschedulable)
	}
	return lastAssignments, pm.preemptedVictims(), nil
}

// simPreemptionVictim adapts a PreemptionVictim to the framework's PreemptionVictim interface.
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

// TODO: Support PodDisruptionBudget violation counting during preemption simulation,
// matching kube-scheduler's FilterVictimsWithPDBViolation behavior.
func (v *simPreemptionVictim) NumPDBViolations() int {
	return 0
}

// prefixPreemptionManager provides candidate victims and creates a ReprieveFilter
// for prefix-search and reprieval preemption simulations.
type prefixPreemptionManager struct {
	NoopPreemptionManager
	victims          []*simPreemptionVictim
	cursor           int
	filterCursor     int
	preemptionFilter PreemptionFilter
	allowReprieve    bool
}

var _ upstreampreemption.PreemptionManager = (*prefixPreemptionManager)(nil)

func newPrefixPreemptionManager(filter PreemptionFilter) *prefixPreemptionManager {
	return &prefixPreemptionManager{
		preemptionFilter: filter,
	}
}

func (pm *prefixPreemptionManager) addVictim(sv *simPreemptionVictim) {
	pm.victims = append(pm.victims, sv)
	pm.cursor++
}

func (pm *prefixPreemptionManager) syncFilter(target int) {
	if pm.preemptionFilter == nil {
		pm.filterCursor = target
		return
	}
	for pm.filterCursor < target {
		pm.preemptionFilter.PreemptVictim(pm.victims[pm.filterCursor].victim)
		pm.filterCursor++
	}
}

func (pm *prefixPreemptionManager) GenerateVictims(
	_ context.Context,
	_ fwk.PodGroupInfo,
) ([]upstreampreemption.PreemptionVictim, *fwk.Status) {
	res := make([]upstreampreemption.PreemptionVictim, pm.cursor)
	for i := range pm.cursor {
		res[i] = pm.victims[i]
	}
	return res, nil
}

func (pm *prefixPreemptionManager) NewReprieveFilter(
	_ context.Context,
	_ []upstreampreemption.PreemptionVictim,
) upstreampreemption.ReprieveFilter {
	return &workloadReprieveFilter{
		allowReprieve: pm.allowReprieve,
		filter:        pm.preemptionFilter,
	}
}

func (pm *prefixPreemptionManager) preemptedVictims() []PreemptionVictim {
	var res []PreemptionVictim
	for _, sv := range pm.victims[:pm.cursor] {
		if !sv.reprieved {
			res = append(res, sv.victim)
		}
	}
	return res
}

// workloadReprieveFilter evaluates and tracks victim reprieval using PreemptionFilter.
type workloadReprieveFilter struct {
	allowReprieve           bool
	filter                  PreemptionFilter
	previousVictimToPreempt PreemptionVictim
}

var _ upstreampreemption.ReprieveFilter = (*workloadReprieveFilter)(nil)

func (rf *workloadReprieveFilter) ShouldAttemptReprieval(
	_ context.Context,
	victim upstreampreemption.PreemptionVictim,
) (bool, error) {
	if !rf.allowReprieve {
		return false, nil
	}
	if rf.filter == nil {
		return true, nil
	}
	if rf.previousVictimToPreempt != nil {
		rf.filter.PreemptVictim(rf.previousVictimToPreempt)
		rf.previousVictimToPreempt = nil
	}
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return false, fmt.Errorf("unexpected PreemptionVictim type %T", victim)
	}
	rf.filter.UnpreemptVictim(sv.victim)
	if !rf.filter.MayFit() {
		rf.filter.PreemptVictim(sv.victim)
		return false, nil
	}
	rf.previousVictimToPreempt = sv.victim
	return true, nil
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
	rf.previousVictimToPreempt = nil
	return nil
}

// NoopPreemptionManager is a no-op implementation of PreemptionManager, PreemptionExecutor, and ReprieveFilter
// used as the default simulation neutralizer when workload preemption is not active.
type NoopPreemptionManager struct{}

var _ upstreampreemption.PreemptionManager = &NoopPreemptionManager{}
var _ upstreampreemption.PreemptionExecutor = &NoopPreemptionManager{}
var _ upstreampreemption.ReprieveFilter = &NoopPreemptionManager{}

// GenerateVictims is a no-op implementation of GenerateVictims from PreemptionManager interface.
func (s *NoopPreemptionManager) GenerateVictims(_ context.Context, _ fwk.PodGroupInfo) ([]upstreampreemption.PreemptionVictim, *fwk.Status) {
	return nil, nil
}

// Executor returns PreemptionExecutor from PreemptionManager interface.
func (s *NoopPreemptionManager) Executor() upstreampreemption.PreemptionExecutor {
	return s
}

// NewReprieveFilter is a no-op implementation of NewReprieveFilter from PreemptionManager interface.
func (s *NoopPreemptionManager) NewReprieveFilter(_ context.Context, _ []upstreampreemption.PreemptionVictim) upstreampreemption.ReprieveFilter {
	return s
}

// IsPodRunningPreemption is a no-op implementation of IsPodRunningPreemption from PreemptionExecutor interface.
func (s *NoopPreemptionManager) IsPodRunningPreemption(_ types.UID) bool {
	return false
}

// IsPodGroupRunningPreemption is a no-op implementation of IsPodGroupRunningPreemption from PreemptionExecutor interface.
func (s *NoopPreemptionManager) IsPodGroupRunningPreemption(_ types.UID) bool {
	return false
}

// IsPodGroupWaitingForVictims is a no-op implementation of IsPodGroupWaitingForVictims from PreemptionExecutor interface.
func (s *NoopPreemptionManager) IsPodGroupWaitingForVictims(_ fwk.PodGroupInfo) bool {
	return false
}

// ActuatePodPreemption is a no-op implementation of ActuatePodPreemption from PreemptionExecutor interface.
func (s *NoopPreemptionManager) ActuatePodPreemption(_ context.Context, _ upstreampreemption.PreemptionCandidate, _ *v1.Pod, _ string) *fwk.Status {
	return nil
}

// ActuatePodGroupPreemption is a no-op implementation of ActuatePodGroupPreemption from PreemptionExecutor interface.
func (s *NoopPreemptionManager) ActuatePodGroupPreemption(_ context.Context, _ upstreampreemption.PreemptionCandidate, _ fwk.PodGroupInfo, _ string) *fwk.Status {
	return nil
}

// ShouldAttemptReprieval is a no-op implementation of ShouldAttemptReprieval from ReprieveFilter interface.
func (s *NoopPreemptionManager) ShouldAttemptReprieval(_ context.Context, _ upstreampreemption.PreemptionVictim) (bool, error) {
	return false, nil
}

// OnVictimReprieved is a no-op implementation of OnVictimReprieved from ReprieveFilter interface.
func (s *NoopPreemptionManager) OnVictimReprieved(_ context.Context, _ upstreampreemption.PreemptionVictim) error {
	return nil
}
