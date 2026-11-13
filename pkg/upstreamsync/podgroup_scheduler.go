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

package upstreamsync

import (
	"errors"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

/*

This file extracts result-completion logic from Scheduler for scheduling PodGroups and CompositePodGroups.

Extracted from kubernetes/kubernetes/pkg/scheduler/schedule_one_podgroup.go and pkg/scheduler/types.go.

The upstream doc comments are kept verbatim so that each copy can be diffed against its origin.
Every intentional deviation is marked with the "UPSTREAM-DIFF:" prefix, either on the declaration
or on the exact line that differs. See pkg/upstreamsync/doc.go and CONTRIBUTING.md.

*/

// PodScheduledResult stores the scheduling result and status for a scheduling attempt of a single pod.
//
// UPSTREAM-DIFF: mirrors scheduler.PodScheduledResult in pkg/scheduler/types.go because its fields are
// unexported and completePodGroupAlgorithmResult is not yet exported in k/k. Stores cycleState directly
// instead of the unexported podSchedulingContext, and omits schedulingDuration.
type PodScheduledResult struct {
	// podInfo is the pod info for the pod the result applies to.
	podInfo fwk.PodInfo
	// scheduleResult is a scheduling algorithm result.
	scheduleResult scheduler.ScheduleResult
	// cycleState is the pod-scoped cycle state used for the scheduling algorithm.
	cycleState fwk.CycleState
	// status is a scheduling algorithm status.
	status *fwk.Status
}

var _ fwk.ProposedAssignment = (*PodScheduledResult)(nil)

// GetPod returns the pod the result applies to.
func (ar *PodScheduledResult) GetPod() *v1.Pod {
	return ar.podInfo.GetPod()
}

// GetPodInfo returns the pod info for the pod the result applies to.
func (ar *PodScheduledResult) GetPodInfo() fwk.PodInfo {
	return ar.podInfo
}

// GetNodeName returns the suggested host for the pod.
func (ar *PodScheduledResult) GetNodeName() string {
	return ar.scheduleResult.SuggestedHost
}

// GetCycleState returns the pod-scoped cycle state used for the scheduling algorithm.
func (ar *PodScheduledResult) GetCycleState() fwk.CycleState {
	return ar.cycleState
}

// Status returns the scheduling algorithm status for the pod.
func (ar *PodScheduledResult) Status() *fwk.Status {
	return ar.status
}

// PodGroupScheduledResult stores the pod scheduling results for a pod group
// and any information needed to act on these results.
//
// UPSTREAM-DIFF: mirrors scheduler.PodGroupScheduledResult in pkg/scheduler/types.go because its fields are
// unexported and completePodGroupAlgorithmResult / completeCompositePodGroupAlgorithmResult are not yet
// exported in k/k. Omits waitingOnPreemption as the library handles preemption synchronously.
type PodGroupScheduledResult struct {
	// podGroupInfo is the leaf pod group this result applies to.
	podGroupInfo *framework.PodGroupInfo
	// podResults is the list of scheduling results for each pod in the group.
	// Only in the case of a pod group-wide Unschedulable or Error status can it contain fewer pods.
	podResults []PodScheduledResult
	// status is the final status of the pod group algorithm.
	status *fwk.Status
	// placementCycleState is the state with which this placement was processed.
	placementCycleState fwk.PlacementCycleState
	// anyScheduled indicates if at least one pod was scheduled in this pod group during this cycle.
	anyScheduled bool
}

// PodGroupInfo returns the pod group this result applies to.
func (r *PodGroupScheduledResult) PodGroupInfo() *framework.PodGroupInfo {
	return r.podGroupInfo
}

// PodResults returns the list of scheduling results for each pod in the group.
func (r *PodGroupScheduledResult) PodResults() []PodScheduledResult {
	return r.podResults
}

// Status returns the final status of the pod group algorithm.
func (r *PodGroupScheduledResult) Status() *fwk.Status {
	return r.status
}

// PlacementCycleState returns the state with which this placement was processed.
func (r *PodGroupScheduledResult) PlacementCycleState() fwk.PlacementCycleState {
	return r.placementCycleState
}

// AnyScheduled returns whether at least one pod was scheduled in this pod group during this cycle.
func (r *PodGroupScheduledResult) AnyScheduled() bool {
	return r.anyScheduled
}

// fromUpstreamPodGroupScheduledResult converts an upstream scheduler.PodGroupScheduledResult into
// a mutable upstreamsync.PodGroupScheduledResult so that CompletePodGroupAlgorithmResult and
// CompleteCompositePodGroupAlgorithmResult can populate skipped pods and propagate ancestor failure statuses.
//
// UPSTREAM-DIFF: conversion helper needed only until CompletePodGroupAlgorithmResult and
// CompleteCompositePodGroupAlgorithmResult are exported in k/k.
func fromUpstreamPodGroupScheduledResult(upstream *scheduler.PodGroupScheduledResult) *PodGroupScheduledResult {
	upstreamPodResults := upstream.PodResults()
	podResults := make([]PodScheduledResult, len(upstreamPodResults))
	for i := range upstreamPodResults {
		pRes := &upstreamPodResults[i]
		podResults[i] = PodScheduledResult{
			podInfo:        pRes.GetPodInfo(),
			scheduleResult: scheduler.ScheduleResult{SuggestedHost: pRes.GetNodeName()},
			cycleState:     pRes.GetCycleState(),
			status:         pRes.Status(),
		}
	}
	return &PodGroupScheduledResult{
		podGroupInfo:        upstream.PodGroupInfo(),
		podResults:          podResults,
		status:              upstream.Status(),
		placementCycleState: upstream.PlacementCycleState(),
		anyScheduled:        upstream.AnyScheduled(),
	}
}

// podGroupFitError describes a fit error for a PodGroup or CompositePodGroup.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/types.go.
type podGroupFitError struct {
	// status is a PodGroup rejection status.
	status *fwk.Status
	// numPlacements is the number of generated placements.
	numPlacements int
	// unschedulablePlugins are plugins that returned Wait, Unschedulable or UnschedulableAndUnresolvable status.
	unschedulablePlugins sets.Set[string]
	// pendingPlugins are plugins that returned Pending status.
	pendingPlugins sets.Set[string]
}

// newPodGroupFitError creates a new PodGroupFitError without placement context.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/types.go.
func newPodGroupFitError(s *fwk.Status) *podGroupFitError {
	return newPodGroupPlacementFitError(s, 0)
}

// newPodGroupPlacementFitError creates a new PodGroupFitError within placement algorithm.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/types.go.
func newPodGroupPlacementFitError(s *fwk.Status, numPlacements int) *podGroupFitError {
	fe := &podGroupFitError{
		status:        s,
		numPlacements: numPlacements,
	}
	fe.addPluginStatus(s)
	return fe
}

// addPluginStatus updates the fit error with plugin status.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/types.go.
func (fe *podGroupFitError) addPluginStatus(s *fwk.Status) {
	if fitError, ok := errors.AsType[*podGroupFitError](s.AsError()); ok {
		// If the status holds podGroupFitError, it means that it already contains
		// the unschedulablePlugins and pendingPlugins inside.
		fe.unschedulablePlugins = fe.unschedulablePlugins.Union(fitError.unschedulablePlugins)
		fe.pendingPlugins = fe.pendingPlugins.Union(fitError.pendingPlugins)
		return
	}
	if s.Plugin() == "" {
		return
	}
	if s.IsRejected() || s.IsWait() {
		if fe.unschedulablePlugins == nil {
			fe.unschedulablePlugins = sets.New[string]()
		}
		fe.unschedulablePlugins.Insert(s.Plugin())
	}
	if s.Code() == fwk.Pending {
		if fe.pendingPlugins == nil {
			fe.pendingPlugins = sets.New[string]()
		}
		fe.pendingPlugins.Insert(s.Plugin())
	}
}

// Error returns the error message for the fit error.
// In case of non-placement algorithm's error, it will not contain any placement context.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/types.go.
func (fe *podGroupFitError) Error() string {
	reason := fe.status.Message()
	if fe.numPlacements == 0 {
		return reason
	}
	return fmt.Sprintf("0/%d placements are available, first placement status: %s", fe.numPlacements, reason)
}

// CompletePodGroupAlgorithmResult ensures that the PodGroupScheduledResult contains the same number of podResults as there are queued pods in the pod group.
//
// UPSTREAM-DIFF: adapted from completePodGroupAlgorithmResult in pkg/scheduler/schedule_one_podgroup.go;
// exported for use by pkg/upstreamsync/snapshot until exported in k/k.
// Converts *scheduler.PodGroupScheduledResult to *PodGroupScheduledResult (since upstream's struct fields are unexported)
// and omits ctx.
func CompletePodGroupAlgorithmResult(
	queuedPodInfos []*framework.QueuedPodInfo,
	podGroupState *framework.CycleState,
	podGroupResult *scheduler.PodGroupScheduledResult,
) *PodGroupScheduledResult {
	return completePodGroupAlgorithmResult(queuedPodInfos, podGroupState, fromUpstreamPodGroupScheduledResult(podGroupResult))
}

// completePodGroupAlgorithmResult ensures that the PodGroupScheduledResult contains the same number of podResults as there are queued pods in the pod group.
//
// UPSTREAM-DIFF: adapted from completePodGroupAlgorithmResult in pkg/scheduler/schedule_one_podgroup.go;
// omits ctx and initializes pod CycleState directly instead of podSchedulingContext.
func completePodGroupAlgorithmResult(
	queuedPodInfos []*framework.QueuedPodInfo,
	podGroupState *framework.CycleState,
	podGroupResult *PodGroupScheduledResult,
) *PodGroupScheduledResult {
	numInResult := len(podGroupResult.podResults)
	numInQueue := len(queuedPodInfos)
	// UPSTREAM-DIFF: upstream currently overwrites successful pod statuses with the failed
	// pod group status in submitPodGroupResult; done here as the library has no submit phase.
	if !podGroupResult.status.IsSuccess() {
		for i := range podGroupResult.podResults {
			if podGroupResult.status.IsError() {
				if !podGroupResult.podResults[i].status.IsError() {
					podGroupResult.podResults[i].status = buildParentPodGroupErrorStatus(podGroupResult.podGroupInfo.GetName())
				}
			} else if podGroupResult.podResults[i].status.IsSuccess() {
				podGroupResult.podResults[i].status = podGroupResult.status.Clone()
			}
		}
	}
	if numInResult == numInQueue {
		return podGroupResult
	}
	skippedPodStatus := podGroupResult.status
	if skippedPodStatus.IsError() {
		skippedPodStatus = buildParentPodGroupErrorStatus(podGroupResult.podGroupInfo.GetName())
	}
	newResults := make([]PodScheduledResult, numInQueue)
	copy(newResults, podGroupResult.podResults)
	for i := numInResult; i < numInQueue; i++ {
		pInfo := queuedPodInfos[i]
		placementCycleState := framework.NewCycleState()
		placementCycleState.SetPodGroupCycleState(podGroupState)
		podCycleState := framework.NewCycleState()
		podCycleState.SetPodGroupCycleState(podGroupState)
		podCycleState.SetPlacementCycleState(placementCycleState)
		newResults[i] = PodScheduledResult{
			podInfo:    pInfo,
			cycleState: podCycleState,
			status:     skippedPodStatus.Clone(),
		}
	}
	podGroupResult.podResults = newResults
	return podGroupResult
}

// CompleteCompositePodGroupAlgorithmResult post-processes scheduling results for a composite pod group.
// It ensures that every pod in every subgroup has a fully populated status and that failure statuses
// are propagated down the tree before finalizing the cycle.
//
// UPSTREAM-DIFF: adapted from completeCompositePodGroupAlgorithmResult in pkg/scheduler/schedule_one_podgroup.go;
// exported for use by pkg/upstreamsync/snapshot until exported in k/k.
// Converts map[fwk.EntityKey]*scheduler.PodGroupScheduledResult to map[fwk.EntityKey]*PodGroupScheduledResult
// (since upstream's struct fields are unexported) and omits ctx.
func CompleteCompositePodGroupAlgorithmResult(
	rootPodGroupInfo *framework.QueuedPodGroupInfo,
	rootCycleState *framework.CycleState,
	upstreamResults map[fwk.EntityKey]*scheduler.PodGroupScheduledResult,
) map[fwk.EntityKey]*PodGroupScheduledResult {
	pgResults := make(map[fwk.EntityKey]*PodGroupScheduledResult, len(upstreamResults))
	for k, v := range upstreamResults {
		pgResults[k] = fromUpstreamPodGroupScheduledResult(v)
	}
	completeCompositePodGroupAlgorithmResultMap(rootPodGroupInfo.PodGroupInfo, pgResults, nil)
	for pgKey, queuedPodInfos := range rootPodGroupInfo.ForEachGroupAndPodInfos() {
		pgResult := pgResults[pgKey]
		// Ensure podResults has an entry for each pod in the pod group with a status.
		completePodGroupAlgorithmResult(queuedPodInfos, rootCycleState, pgResult)
	}
	return pgResults
}

// completeCompositePodGroupAlgorithmResultMap propagates failures of composite pod groups to their
// descendants, so that no group is bound while one of its ancestors failed:
//   - A group that succeeded or was not evaluated gets a status derived from its nearest failed
//     ancestor (see buildAncestorFailureStatus).
//   - A group that failed on its own keeps its status, unless the ancestor failed with an Error
//     and the group did not, in which case the ancestor's Error takes precedence.
//   - A group that keeps its own failure becomes the nearest failed ancestor of its subtree.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/schedule_one_podgroup.go.
func completeCompositePodGroupAlgorithmResultMap(
	podGroupInfo *framework.PodGroupInfo,
	pgResults map[fwk.EntityKey]*PodGroupScheduledResult,
	failedAncestor *PodGroupScheduledResult,
) {
	key := podGroupInfo.GetKey()
	result, evaluated := pgResults[key]
	if !evaluated {
		// Groups are only skipped after an ancestor failed.
		result = &PodGroupScheduledResult{podGroupInfo: podGroupInfo}
		pgResults[key] = result
	}
	if failedAncestor != nil {
		if !evaluated || result.status.IsSuccess() {
			// The group can't be bound while its ancestor failed.
			result.status = buildAncestorFailureStatus(failedAncestor)
		} else if failedAncestor.status.IsError() && !result.status.IsError() {
			// An error takes precedence over the group's own non-error failure.
			result.status = buildAncestorFailureStatus(failedAncestor)
		} else {
			// The group's own failure is the more specific reason, so its subtree inherits it.
			failedAncestor = result
		}
	} else if !result.status.IsSuccess() {
		// The subtree inherits the group's own failure.
		failedAncestor = result
	}

	for _, child := range podGroupInfo.Children() {
		completeCompositePodGroupAlgorithmResultMap(child, pgResults, failedAncestor)
	}
}

// buildParentPodGroupErrorStatus returns the status for a pod whose parent pod group encountered an error.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/schedule_one_podgroup.go.
func buildParentPodGroupErrorStatus(podGroupName string) *fwk.Status {
	return fwk.AsStatus(fmt.Errorf("parent pod group %q encountered an error", podGroupName))
}

// buildAncestorFailureStatus returns the status for a pod group whose ancestor failed.
// The status only names the ancestor, without repeating the ancestor's failure reason,
// which is reported in the ancestor's own status.
// If the ancestor's status is an Error, it returns an Error.
// Otherwise, it returns ancestor's status code with a podGroupFitError that names the ancestor
// and carries its plugins.
//
// UPSTREAM-DIFF: none, copied verbatim from pkg/scheduler/schedule_one_podgroup.go.
func buildAncestorFailureStatus(failedAncestor *PodGroupScheduledResult) *fwk.Status {
	ancestorName := failedAncestor.podGroupInfo.GetName()
	if failedAncestor.status.IsError() {
		return fwk.AsStatus(fmt.Errorf("ancestor composite pod group %q encountered an error", ancestorName))
	}

	msg := fmt.Sprintf("ancestor composite pod group %q is unschedulable", ancestorName)
	fitError := newPodGroupFitError(fwk.NewStatus(fwk.Unschedulable, msg))
	fitError.addPluginStatus(failedAncestor.status)
	return fwk.NewStatus(failedAncestor.status.Code()).WithError(fitError)
}
