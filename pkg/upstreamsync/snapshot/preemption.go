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

package snapshot

import (
	"context"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// PreemptionManager implements preemption.PreemptionManager for a ClusterSnapshot
// by delegating per-call preemption operations to the active workloadPreemptor.
type PreemptionManager struct {
	preemption.NoopPreemptionManager
	wp *workloadPreemptor
}

var _ preemption.PreemptionManager = (*PreemptionManager)(nil)

// NewPreemptionManager creates a new PreemptionManager for a ClusterSnapshot.
func NewPreemptionManager() *PreemptionManager {
	return &PreemptionManager{}
}

// Factory returns a preemption.PreemptionManagerFactory that binds all profiles
// in a ProfileMap to this PreemptionManager.
func (pm *PreemptionManager) Factory() preemption.PreemptionManagerFactory {
	return func() preemption.PreemptionManager {
		return pm
	}
}

// GenerateVictims delegates candidate victim generation to the active workloadPreemptor.
func (pm *PreemptionManager) GenerateVictims(ctx context.Context, pgInfo fwk.PodGroupInfo) ([]preemption.PreemptionVictim, *fwk.Status) {
	if pm.wp == nil {
		return nil, nil
	}
	return pm.wp.generateVictims(ctx, pgInfo)
}

// NewReprieveFilter delegates ReprieveFilter creation to the active workloadPreemptor.
func (pm *PreemptionManager) NewReprieveFilter(ctx context.Context, allVictims []preemption.PreemptionVictim) preemption.ReprieveFilter {
	if pm.wp == nil {
		return pm.NoopPreemptionManager.NewReprieveFilter(ctx, allVictims)
	}
	return pm.wp.newReprieveFilter(ctx, allVictims)
}

// simPreemptionVictim wraps a caller-provided PreemptionVictim to satisfy
// preemption.PreemptionVictim while tracking whether the victim was reprieved.
type simPreemptionVictim struct {
	victim    PreemptionVictim
	podInfos  []fwk.PodInfo
	reprieved bool
}

var _ preemption.PreemptionVictim = (*simPreemptionVictim)(nil)

func (v *simPreemptionVictim) Pods() []fwk.PodInfo {
	return v.podInfos
}

func (v *simPreemptionVictim) NumPDBViolations() int {
	return 0
}

// workloadReprieveFilter bridges preemption.ReprieveFilter to the caller's PreemptionFilter.
type workloadReprieveFilter struct {
	allowReprieve    bool
	preemptionFilter PreemptionFilter
}

var _ preemption.ReprieveFilter = (*workloadReprieveFilter)(nil)

func (f *workloadReprieveFilter) ShouldAttemptReprieval(_ context.Context, _ preemption.PreemptionVictim) (bool, error) {
	// TODO: implement quota pre-check via f.preemptionFilter.
	return f.allowReprieve, nil
}

func (f *workloadReprieveFilter) OnVictimReprieved(_ context.Context, victim preemption.PreemptionVictim) error {
	// TODO: mark victim as reprieved and update f.preemptionFilter.
	if sv, ok := victim.(*simPreemptionVictim); ok {
		sv.reprieved = true
		if f.preemptionFilter != nil {
			f.preemptionFilter.UnpreemptVictim(sv.victim)
		}
	}
	return nil
}

// workloadPreemptor manages per-ScheduleWorkload preemption state, collecting
// PotentialVictims during exponential search and keeping PreemptionFilter
// synchronized with the active prefix [0:cursor].
type workloadPreemptor struct {
	snapshot         *cache.Snapshot
	victims          []*simPreemptionVictim
	cursor           int
	preemptionFilter PreemptionFilter
	allowReprieve    bool
}

func newWorkloadPreemptor(snap *cache.Snapshot, opts WorkloadPreemptionOptions) *workloadPreemptor {
	return &workloadPreemptor{
		snapshot:         snap,
		preemptionFilter: opts.PreemptionFilter,
	}
}

func (wp *workloadPreemptor) addVictim(v PreemptionVictim) error {
	// TODO: resolve PodInfos from wp.snapshot for v.Pods().
	wp.victims = append(wp.victims, &simPreemptionVictim{
		victim: v,
	})
	wp.cursor++
	if wp.preemptionFilter != nil {
		wp.preemptionFilter.PreemptVictim(v)
	}
	return nil
}

func (wp *workloadPreemptor) setCursor(target int) {
	for wp.cursor < target {
		if wp.preemptionFilter != nil {
			wp.preemptionFilter.PreemptVictim(wp.victims[wp.cursor].victim)
		}
		wp.cursor++
	}
	for wp.cursor > target {
		wp.cursor--
		if wp.preemptionFilter != nil {
			wp.preemptionFilter.UnpreemptVictim(wp.victims[wp.cursor].victim)
		}
	}
}

func (wp *workloadPreemptor) generateVictims(_ context.Context, _ fwk.PodGroupInfo) ([]preemption.PreemptionVictim, *fwk.Status) {
	result := make([]preemption.PreemptionVictim, wp.cursor)
	for i := 0; i < wp.cursor; i++ {
		wp.victims[i].reprieved = false
		result[i] = wp.victims[i]
	}
	return result, fwk.NewStatus(fwk.Success)
}

func (wp *workloadPreemptor) newReprieveFilter(_ context.Context, _ []preemption.PreemptionVictim) preemption.ReprieveFilter {
	return &workloadReprieveFilter{
		allowReprieve:    wp.allowReprieve,
		preemptionFilter: wp.preemptionFilter,
	}
}

// findPreemptionVictims orchestrates the two-stage workload preemption evaluation
// (prefix search followed by reverse-order reprieval).
func (s *ClusterSnapshot) findPreemptionVictims(
	ctx context.Context,
	sched *upstreamsync.Scheduler,
	schedFramework *preemption.PreemptionFramework,
	podGroupInfo *framework.PodGroupInfo,
	preemptionOpts WorkloadPreemptionOptions,
	unschedulableResult WorkloadSchedulingResult,
) WorkloadSchedulingResult {
	wp := newWorkloadPreemptor(s.schedulerSnapshot, preemptionOpts)
	if s.preemptionManager != nil {
		s.preemptionManager.wp = wp
	}
	defer func() {
		if s.preemptionManager != nil {
			s.preemptionManager.wp = nil
		}
	}()

	// Step 1: Evict CommittedVictims and check if they alone satisfy the workload.
	if result, ok := s.preemptCommittedVictims(ctx, sched, schedFramework, podGroupInfo, preemptionOpts, unschedulableResult); ok {
		return result
	}

	// Step 2: Run Exponential + Binary Search with allowReprieve = false to find minimal prefix k*.
	kStar, ok, err := s.searchMinimalVictimPrefix(ctx, sched, schedFramework, podGroupInfo, wp, preemptionOpts)
	if err != nil {
		return WorkloadSchedulingResult{Status: fwk.AsStatus(err)}
	}
	if !ok {
		return unschedulableResult
	}

	// Step 3: Run final evaluation on V[0:k*] with allowReprieve = true to spare unneeded victims.
	return s.reprieveAndEvaluateVictims(ctx, sched, schedFramework, podGroupInfo, wp, kStar, preemptionOpts, unschedulableResult)
}

func (s *ClusterSnapshot) preemptCommittedVictims(
	_ context.Context,
	_ *upstreamsync.Scheduler,
	_ *preemption.PreemptionFramework,
	_ *framework.PodGroupInfo,
	_ WorkloadPreemptionOptions,
	_ WorkloadSchedulingResult,
) (WorkloadSchedulingResult, bool) {
	// TODO: remove CommittedVictims from snapshot, notify PreemptionFilter, and check if workload fits.
	return WorkloadSchedulingResult{}, false
}

func (s *ClusterSnapshot) searchMinimalVictimPrefix(
	_ context.Context,
	_ *upstreamsync.Scheduler,
	_ *preemption.PreemptionFramework,
	_ *framework.PodGroupInfo,
	wp *workloadPreemptor,
	opts WorkloadPreemptionOptions,
) (int, bool, error) {
	// TODO: implement Exponential + Binary Search over PotentialVictims with wp.allowReprieve = false.
	if opts.PotentialVictims == nil {
		return 0, false, nil
	}
	for v := range opts.PotentialVictims {
		if err := wp.addVictim(v); err != nil {
			return 0, false, err
		}
		break
	}
	return 0, false, nil
}

func (s *ClusterSnapshot) reprieveAndEvaluateVictims(
	_ context.Context,
	_ *upstreamsync.Scheduler,
	_ *preemption.PreemptionFramework,
	_ *framework.PodGroupInfo,
	wp *workloadPreemptor,
	kStar int,
	_ WorkloadPreemptionOptions,
	unschedulableResult WorkloadSchedulingResult,
) WorkloadSchedulingResult {
	// TODO: run RunPodGroupPostFilterPlugins on V[0:kStar] with wp.allowReprieve = true and collect victims.
	wp.setCursor(kStar)
	return unschedulableResult
}
