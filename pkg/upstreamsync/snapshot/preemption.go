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
	"iter"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// simPreemptionVictim adapts a snapshot.PreemptionVictim to preemption.PreemptionVictim.
type simPreemptionVictim struct {
	victim    PreemptionVictim
	podInfos  []fwk.PodInfo
	reprieved bool
}

var _ preemption.PreemptionVictim = (*simPreemptionVictim)(nil)

func newSimPreemptionVictim(v PreemptionVictim) (*simPreemptionVictim, error) {
	if v == nil {
		return nil, fmt.Errorf("preemption victim cannot be nil")
	}
	pods := v.Pods()
	podInfos := make([]fwk.PodInfo, 0, len(pods))
	for _, pod := range pods {
		if pod == nil {
			return nil, fmt.Errorf("preemption victim pod cannot be nil")
		}
		if pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("preemption victim pod %s/%s is not assigned to a node", pod.Namespace, pod.Name)
		}
		pi, err := framework.NewPodInfo(pod.DeepCopy())
		if err != nil {
			return nil, fmt.Errorf("failed to create PodInfo for victim pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		podInfos = append(podInfos, pi)
	}
	return &simPreemptionVictim{
		victim:   v,
		podInfos: podInfos,
	}, nil
}

func (v *simPreemptionVictim) Pods() []fwk.PodInfo {
	return v.podInfos
}

func (v *simPreemptionVictim) NumPDBViolations() int {
	return 0
}

// workloadPreemptor implements preemption.PreemptionManager and preemption.ReprieveFilter
// for a single ScheduleWorkload invocation.
type workloadPreemptor struct {
	preemption.NoopPreemptionManager
	nextVictim       func() (PreemptionVictim, bool)
	stopIter         func()
	iterExhausted    bool
	victims          []*simPreemptionVictim
	preemptionFilter PreemptionFilter
}

var _ preemption.PreemptionManager = (*workloadPreemptor)(nil)
var _ preemption.ReprieveFilter = (*workloadPreemptor)(nil)

func newWorkloadPreemptor(opts WorkloadPreemptionOptions) *workloadPreemptor {
	var next func() (PreemptionVictim, bool)
	var stop func()
	if opts.PotentialVictims != nil {
		next, stop = iter.Pull(opts.PotentialVictims)
	}
	return &workloadPreemptor{
		nextVictim:       next,
		stopIter:         stop,
		iterExhausted:    opts.PotentialVictims == nil,
		preemptionFilter: opts.PreemptionFilter,
	}
}

func (wp *workloadPreemptor) close() {
	if wp.stopIter != nil {
		wp.stopIter()
		wp.stopIter = nil
	}
}

// prepareNextTry pulls victims from PotentialVictims until PreemptionFilter.MayFit returns true.
// It returns true if a new prefix is ready for evaluation, or false if the iterator is exhausted.
func (wp *workloadPreemptor) prepareNextTry() (bool, error) {
	if wp.iterExhausted || wp.nextVictim == nil {
		return false, nil
	}
	for {
		v, ok := wp.nextVictim()
		if !ok {
			wp.iterExhausted = true
			return false, nil
		}
		sv, err := newSimPreemptionVictim(v)
		if err != nil {
			return false, err
		}
		wp.victims = append(wp.victims, sv)
		if wp.preemptionFilter != nil {
			wp.preemptionFilter.PreemptVictim(v)
			if !wp.preemptionFilter.MayFit() {
				continue
			}
		}
		return true, nil
	}
}

func (wp *workloadPreemptor) GenerateVictims(
	_ context.Context,
	_ fwk.PodGroupInfo,
) ([]preemption.PreemptionVictim, *fwk.Status) {
	res := make([]preemption.PreemptionVictim, len(wp.victims))
	for i, sv := range wp.victims {
		res[i] = sv
	}
	return res, fwk.NewStatus(fwk.Success)
}

func (wp *workloadPreemptor) NewReprieveFilter(
	_ context.Context,
	_ []preemption.PreemptionVictim,
) preemption.ReprieveFilter {
	return wp
}

func (wp *workloadPreemptor) ShouldAttemptReprieval(
	_ context.Context,
	victim preemption.PreemptionVictim,
) (bool, error) {
	if wp.preemptionFilter == nil {
		return true, nil
	}
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return false, fmt.Errorf("unexpected PreemptionVictim type %T", victim)
	}
	wp.preemptionFilter.UnpreemptVictim(sv.victim)
	mayFit := wp.preemptionFilter.MayFit()
	wp.preemptionFilter.PreemptVictim(sv.victim)
	return mayFit, nil
}

func (wp *workloadPreemptor) OnVictimReprieved(
	_ context.Context,
	victim preemption.PreemptionVictim,
) error {
	sv, ok := victim.(*simPreemptionVictim)
	if !ok {
		return fmt.Errorf("unexpected PreemptionVictim type %T", victim)
	}
	sv.reprieved = true
	if wp.preemptionFilter != nil {
		wp.preemptionFilter.UnpreemptVictim(sv.victim)
	}
	return nil
}

func (wp *workloadPreemptor) preemptedVictims() []PreemptionVictim {
	var res []PreemptionVictim
	for _, sv := range wp.victims {
		if !sv.reprieved {
			res = append(res, sv.victim)
		}
	}
	return res
}

// collectProposedAssignments extracts fwk.ProposedAssignment pointers in deterministic
// scheduling order across single PodGroups and hierarchical CompositePodGroups.
func collectProposedAssignments(
	rootPGI *framework.PodGroupInfo,
	algResultsMap map[fwk.EntityKey]*upstreamsync.PodGroupAlgorithmResult,
) []fwk.ProposedAssignment {
	var proposed []fwk.ProposedAssignment
	var walk func(pgi *framework.PodGroupInfo)
	walk = func(pgi *framework.PodGroupInfo) {
		res, ok := algResultsMap[getEntityKey(pgi)]
		if !ok || res == nil || !res.Status.IsSuccess() {
			return
		}
		for _, child := range pgi.Children {
			walk(child)
		}
		for i := range res.PodResults {
			if res.PodResults[i].GetStatus().IsSuccess() && res.PodResults[i].GetNodeName() != "" {
				proposed = append(proposed, &res.PodResults[i])
			}
		}
	}
	walk(rootPGI)
	return proposed
}

func collectWorkloadResults(
	rootPGI *framework.PodGroupInfo,
	algResultsMap map[fwk.EntityKey]*upstreamsync.PodGroupAlgorithmResult,
	rootResult *upstreamsync.PodGroupAlgorithmResult,
	dryRun bool,
) []SchedulingResult {
	isRootSuccess := rootResult.Status.IsSuccess()
	var results []SchedulingResult
	var walk func(pgi *framework.PodGroupInfo)
	walk = func(pgi *framework.PodGroupInfo) {
		if groupResult, ok := algResultsMap[getEntityKey(pgi)]; ok && groupResult != nil {
			for _, pRes := range groupResult.PodResults {
				status := pRes.GetStatus()
				nodeName := pRes.GetNodeName()
				pod := pRes.GetPod()
				if isRootSuccess && status.IsSuccess() {
					if dryRun {
						pod = pod.DeepCopy()
					}
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
		for _, child := range pgi.Children {
			walk(child)
		}
	}
	walk(rootPGI)
	return results
}
