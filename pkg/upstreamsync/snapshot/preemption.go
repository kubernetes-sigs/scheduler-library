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
	"fmt"

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

var _ preemption.PreemptionVictim = (*simPreemptionVictim)(nil)

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
	preemption.NoopPreemptionManager
	victims          []*simPreemptionVictim
	preemptionFilter PreemptionFilter
	actuated         bool
}

var _ preemption.PreemptionManager = (*prefixPreemptionManager)(nil)
var _ preemption.PreemptionExecutor = (*prefixPreemptionManager)(nil)

func newPrefixPreemptionManager(victims []*simPreemptionVictim, filter PreemptionFilter) *prefixPreemptionManager {
	return &prefixPreemptionManager{
		victims:          victims,
		preemptionFilter: filter,
	}
}

func (pm *prefixPreemptionManager) Executor() preemption.PreemptionExecutor {
	return pm
}

func (pm *prefixPreemptionManager) ActuatePodGroupPreemption(_ context.Context, _ preemption.PreemptionCandidate, _ fwk.PodGroupInfo, _ string) *fwk.Status {
	pm.actuated = true
	return nil
}

func (pm *prefixPreemptionManager) GenerateVictims(
	_ context.Context,
	_ fwk.PodGroupInfo,
) ([]preemption.PreemptionVictim, *fwk.Status) {
	res := make([]preemption.PreemptionVictim, len(pm.victims))
	for i, sv := range pm.victims {
		res[i] = sv
	}
	return res, fwk.NewStatus(fwk.Success)
}

func (pm *prefixPreemptionManager) NewReprieveFilter(
	_ context.Context,
	_ []preemption.PreemptionVictim,
) preemption.ReprieveFilter {
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

var _ preemption.ReprieveFilter = (*workloadReprieveFilter)(nil)

func (rf *workloadReprieveFilter) ShouldAttemptReprieval(
	_ context.Context,
	victim preemption.PreemptionVictim,
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
	victim preemption.PreemptionVictim,
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
