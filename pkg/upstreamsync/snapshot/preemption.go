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
	"iter"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// simPreemptionVictim adapts a snapshot.PreemptionVictim to preemption.PreemptionVictim.
type simPreemptionVictim struct {
	victim    PreemptionVictim
	pods      []fwk.PodInfo
	reprieved bool
}

var _ preemption.PreemptionVictim = &simPreemptionVictim{}

func (v *simPreemptionVictim) Pods() []fwk.PodInfo {
	return v.pods
}

func (v *simPreemptionVictim) NumPDBViolations() int {
	return 0
}

// workloadPreemptor implements preemption.PreemptionManager, preemption.PreemptionExecutor,
// and preemption.ReprieveFilter for a single ScheduleWorkload call.
type workloadPreemptor struct {
	preemption.NoopPreemptionManager

	nextVictim       func() (PreemptionVictim, bool)
	stopIter         func()
	preemptionFilter PreemptionFilter

	rawVictims     []PreemptionVictim
	currentVictims []*simPreemptionVictim
	actuated       bool
}

var _ preemption.PreemptionManager = &workloadPreemptor{}
var _ preemption.PreemptionExecutor = &workloadPreemptor{}
var _ preemption.ReprieveFilter = &workloadPreemptor{}

func newWorkloadPreemptor(opts WorkloadPreemptionOptions) *workloadPreemptor {
	next, stop := iter.Pull(opts.PotentialVictims)
	return &workloadPreemptor{
		nextVictim:       next,
		stopIter:         stop,
		preemptionFilter: opts.PreemptionFilter,
	}
}

func (wp *workloadPreemptor) close() {
	if wp.stopIter != nil {
		wp.stopIter()
	}
}

// prepareNextTry pulls victims from PotentialVictims until PreemptionFilter is nil or MayFit returns true.
// It returns true when at least one new victim is added and the filter reports that the workload fits.
func (wp *workloadPreemptor) prepareNextTry() bool {
	added := false
	for {
		v, ok := wp.nextVictim()
		if !ok {
			break
		}
		if v == nil || len(v.Pods()) == 0 {
			continue
		}
		added = true
		wp.rawVictims = append(wp.rawVictims, v)
		if wp.preemptionFilter != nil {
			wp.preemptionFilter.PreemptVictim(v)
		}
		if wp.preemptionFilter == nil || wp.preemptionFilter.MayFit() {
			return true
		}
	}
	return added && (wp.preemptionFilter == nil || wp.preemptionFilter.MayFit())
}

func (wp *workloadPreemptor) GenerateVictims(_ context.Context, _ fwk.PodGroupInfo) ([]preemption.PreemptionVictim, *fwk.Status) {
	wp.currentVictims = make([]*simPreemptionVictim, len(wp.rawVictims))
	res := make([]preemption.PreemptionVictim, len(wp.rawVictims))
	for i, rv := range wp.rawVictims {
		rawPods := rv.Pods()
		podInfos := make([]fwk.PodInfo, 0, len(rawPods))
		for _, p := range rawPods {
			pi, err := framework.NewPodInfo(p)
			if err != nil {
				return nil, fwk.AsStatus(err)
			}
			podInfos = append(podInfos, pi)
		}
		sv := &simPreemptionVictim{
			victim: rv,
			pods:   podInfos,
		}
		wp.currentVictims[i] = sv
		res[i] = sv
	}
	return res, nil
}

func (wp *workloadPreemptor) Executor() preemption.PreemptionExecutor {
	return wp
}

func (wp *workloadPreemptor) NewReprieveFilter(_ context.Context, _ []preemption.PreemptionVictim) preemption.ReprieveFilter {
	return wp
}

func (wp *workloadPreemptor) ShouldAttemptReprieval(_ context.Context, victim preemption.PreemptionVictim) (bool, error) {
	if wp.preemptionFilter == nil {
		return true, nil
	}
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return true, nil
	}
	// Restore the victim in the filter to check if the workload fits,
	// then re-preempt it in case node-level Filter plugins reject the reprieval.
	wp.preemptionFilter.UnpreemptVictim(sv.victim)
	mayFit := wp.preemptionFilter.MayFit()
	wp.preemptionFilter.PreemptVictim(sv.victim)
	return mayFit, nil
}

func (wp *workloadPreemptor) OnVictimReprieved(_ context.Context, victim preemption.PreemptionVictim) error {
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return nil
	}
	sv.reprieved = true
	if wp.preemptionFilter != nil {
		wp.preemptionFilter.UnpreemptVictim(sv.victim)
	}
	return nil
}

func (wp *workloadPreemptor) ActuatePodGroupPreemption(_ context.Context, _ preemption.PreemptionCandidate, _ fwk.PodGroupInfo, _ string) *fwk.Status {
	wp.actuated = true
	return nil
}

func (wp *workloadPreemptor) preemptedVictims() []PreemptionVictim {
	var victims []PreemptionVictim
	for _, sv := range wp.currentVictims {
		if !sv.reprieved {
			victims = append(victims, sv.victim)
		}
	}
	return victims
}

func buildPodGroupSchedulingFunc(
	sched *upstreamsync.Scheduler,
	schedFramework *preemption.PreemptionFramework,
	podGroupInfo *framework.PodGroupInfo,
) fwk.PodGroupSchedulingFunc {
	rootKey := getEntityKey(podGroupInfo)
	return func(ctx context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
		algResultsMap, revertFn := sched.RunRootSchedulingAlgorithm(ctx, schedFramework, framework.NewCycleState(), podGroupInfo)
		// Revert assumed preemptor reservations so reprieveVictim does not
		// add ProposedAssignments on top of already-reserved preemptor pods.
		if revertFn != nil {
			revertFn()
		}

		rootResult := algResultsMap[rootKey]
		if !rootResult.Status.IsSuccess() {
			return nil, rootResult.Status
		}

		var proposed []fwk.ProposedAssignment
		var walk func(pgi *framework.PodGroupInfo)
		walk = func(pgi *framework.PodGroupInfo) {
			groupResult, ok := algResultsMap[getEntityKey(pgi)]
			if !ok || !groupResult.Status.IsSuccess() {
				return
			}
			for _, child := range pgi.Children {
				walk(child)
			}
			for i := range groupResult.PodResults {
				pRes := &groupResult.PodResults[i]
				if pRes.GetStatus().IsSuccess() && pRes.GetNodeName() != "" {
					proposed = append(proposed, pRes)
				}
			}
		}
		walk(podGroupInfo)

		return &fwk.PodGroupAssignments{
			ProposedAssignments: proposed,
		}, rootResult.Status
	}
}

func buildWorkloadPodResults(
	algResultsMap map[fwk.EntityKey]*upstreamsync.PodGroupAlgorithmResult,
	rootResult *upstreamsync.PodGroupAlgorithmResult,
) []SchedulingResult {
	isRootSuccess := rootResult.Status.IsSuccess()
	var results []SchedulingResult
	for _, groupResult := range algResultsMap {
		for _, pRes := range groupResult.PodResults {
			status := pRes.GetStatus()
			nodeName := pRes.GetNodeName()
			pod := pRes.GetPod()
			if isRootSuccess && status.IsSuccess() {
				pod.Spec.NodeName = nodeName
			} else {
				nodeName = ""
				if !isRootSuccess && status.IsSuccess() {
					status = rootResult.Status
				}
			}
			results = append(results, SchedulingResult{
				Pod:              pod,
				Status:           status,
				SelectedNodeName: nodeName,
				CycleState:       pRes.GetCycleState(),
			})
		}
	}
	return results
}
