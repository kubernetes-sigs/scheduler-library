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
	"context"
	"fmt"
	"iter"
	"maps"
	"math/rand"
	"time"

	v1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

/*

This file extracts additional scheduling logic from Scheduler for scheduling PodGroups and CompositePodGroups.

Extracted from kubernetes/kubernetes/pkg/scheduler/schedule_one_podgroup.go.

The upstream doc comments are kept verbatim so that each copy can be diffed against its origin.
Every intentional deviation is marked with the "UPSTREAM-DIFF:" prefix, either on the declaration
or on the exact line that differs. See pkg/upstreamsync/doc.go and CONTRIBUTING.md.

*/

// revertFns is an aggregator of functions that undo the in-memory changes (such
// as assuming pods and calls to Reserve plugins) performed during the pod group
// scheduling algorithm simulation.
//
// UPSTREAM-DIFF: none, copied verbatim.
type revertFns []func()

// Append registers additional revert functions.
//
// UPSTREAM-DIFF: none, copied verbatim.
func (r *revertFns) append(other revertFns) {
	*r = append(*r, other...)
}

// revert executes the underlying reverting functions in reverse order of their registration
// (Last In, First Out). Reverting in LIFO order ensures that sequential operations are unwound
// correctly, preserving state integrity since later operations might depend on the side-effects
// established by earlier ones (similar to how deferred execution works in Go).
//
// UPSTREAM-DIFF: none, copied verbatim.
func (r *revertFns) revert() {
	if r == nil {
		return
	}
	for i := len(*r) - 1; i >= 0; i-- {
		if (*r)[i] != nil {
			(*r)[i]()
			(*r)[i] = nil
		}
	}
	*r = nil
}

// errPodGroupUnschedulable is used to describe that the pod group is unschedulable.
//
// UPSTREAM-DIFF: none, copied verbatim.
var errPodGroupUnschedulable = fmt.Errorf("pod group is unschedulable")

// PodGroupAlgorithmResult stores the pod scheduling results for a pod group
// and any information needed to act on these results.
//
// UPSTREAM-DIFF: replaces the unexported podGroupAlgorithmResult.
// Uses the library's exported AlgorithmResult; selectedPlacement and waitingOnPreemption are dropped
// as the library records no scheduling metrics and handles preemption synchronously.
type PodGroupAlgorithmResult struct {
	PodGroupInfo        *framework.PodGroupInfo
	PodResults          []AlgorithmResult
	Status              *fwk.Status
	PlacementCycleState fwk.PlacementCycleState
	AnyScheduled        bool
}

// RunRootSchedulingAlgorithm orchestrates the scheduling attempt for a root pod group.
// It decides whether to evaluate a single group or recursively evaluate a composite group hierarchy.
// The returned map aggregates scheduling results across the entire pod group hierarchy.
//
// UPSTREAM-DIFF: adapted from Scheduler.runRootSchedulingAlgorithm.
// Unlike upstream (which unconditionally reverts results), scheduler library leaves
// successful reservations intact and returns the revert function for
// callers (e.g. ClusterSnapshot) to control the state.
func (sched *Scheduler) RunRootSchedulingAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	rootPodGroupInfo *framework.PodGroupInfo,
) (results map[fwk.EntityKey]*PodGroupAlgorithmResult, revertFn func()) {
	var revertFns revertFns
	var result *PodGroupAlgorithmResult
	results = make(map[fwk.EntityKey]*PodGroupAlgorithmResult)

	defer func() {
		if revertFn == nil {
			revertFns.revert()
		}
	}()

	// UPSTREAM-DIFF: do not gate on features.CompositePodGroup so composite hierarchies built by
	// ClusterSnapshot.ScheduleWorkload are always evaluated recursively even if the caller did not
	// explicitly set the global CompositePodGroup feature gate.
	if rootPodGroupInfo.GetType() == fwk.CompositePodGroupKeyType {
		result, revertFns = sched.podGroupSchedulingRecursiveAlgorithm(ctx, schedFwk, podGroupCycleState, rootPodGroupInfo, rootPodGroupInfo, results)
	} else {
		result, revertFns = sched.podGroupSchedulingAlgorithm(ctx, schedFwk, podGroupCycleState, rootPodGroupInfo)
		results[getEntityKey(rootPodGroupInfo)] = result
	}

	if result.Status.IsSuccess() && !result.AnyScheduled {
		// The framework requires at least 1 pod to be scheduled in order to return a success status.
		result.Status = fwk.NewStatus(fwk.Unschedulable).WithError(errPodGroupUnschedulable)
	}

	if !result.Status.IsSuccess() {
		return results, nil
	}
	return results, revertFns.revert
}

// BuildPodGroupSchedulingFunc returns a PodGroupSchedulingFunc that runs the root
// pod group scheduling algorithm in a fresh CycleState and collects proposed
// assignments from successful leaf pod groups.
//
// UPSTREAM-DIFF: Adapted from Scheduler.buildPodGroupSchedulingFunc in
// pkg/scheduler/schedule_one_podgroup.go (kubernetes/kubernetes#142979), with one difference:
// Calls revertFn() immediately because RunRootSchedulingAlgorithm in upstreamsync
// returns revertFn to the caller instead of deferring it internally.
// Migration Plan (1.38): Replace with PodGroupSchedulingAlgorithm.BuildPodGroupSchedulingFunc
// once kubernetes/kubernetes#142979 and kubernetes/kubernetes#141852 merge.
func (sched *Scheduler) BuildPodGroupSchedulingFunc(
	schedFwk framework.Framework,
	podGroupInfo *framework.PodGroupInfo,
) fwk.PodGroupSchedulingFunc {
	rootKey := getEntityKey(podGroupInfo)
	return func(ctx context.Context) (*fwk.PodGroupAssignments, *fwk.Status) {
		results, revertFn := sched.RunRootSchedulingAlgorithm(ctx, schedFwk, framework.NewCycleState(), podGroupInfo)
		if revertFn != nil {
			revertFn()
		}
		rootResult := results[rootKey]
		if !rootResult.Status.IsSuccess() {
			return nil, rootResult.Status
		}
		var proposedAssignments []fwk.ProposedAssignment
		for res := range SuccessfulLeafResults(podGroupInfo, results) {
			proposedAssignments = append(proposedAssignments, makeProposedAssignments(res)...)
		}
		return &fwk.PodGroupAssignments{
			ProposedAssignments: proposedAssignments,
		}, rootResult.Status
	}
}

// podGroupSchedulingRecursiveAlgorithm runs a recursive pod group scheduling algorithm.
// If the pod group info wraps a composite pod group, it will recursively invoke the algorithm on its children.
// Otherwise, the pod group info wraps a leaf pod group for which we invoke the standard pod group scheduling algorithm.
// The returned revertFns propagates revert functions from all child pod group evaluations up to the root level.
//
// UPSTREAM-DIFF: adapted from Scheduler.podGroupSchedulingRecursiveAlgorithm.
func (sched *Scheduler) podGroupSchedulingRecursiveAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	root *framework.PodGroupInfo,
	podGroupInfo *framework.PodGroupInfo,
	results map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (*PodGroupAlgorithmResult, revertFns) {
	logger := klog.FromContext(ctx)
	logger.V(5).Info("Running recursive podgroup scheduling algorithm", "rootType", podGroupInfo.Type, "root", klog.KObj(podGroupInfo))

	var algorithmResult *PodGroupAlgorithmResult
	var childRevertFns revertFns
	if podGroupInfo.Type == fwk.PodGroupKeyType {
		algorithmResult, childRevertFns = sched.podGroupSchedulingAlgorithm(ctx, schedFwk, podGroupCycleState, podGroupInfo)
		results[getEntityKey(podGroupInfo)] = algorithmResult
	} else {
		algorithmResult, childRevertFns = sched.compositePodGroupSchedulingAlgorithm(ctx, schedFwk, podGroupCycleState, root, podGroupInfo, results)
	}
	return algorithmResult, childRevertFns
}

// UPSTREAM-DIFF: adapted from Scheduler.compositePodGroupSchedulingAlgorithm.
func (sched *Scheduler) compositePodGroupSchedulingAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	root *framework.PodGroupInfo,
	podGroupInfo *framework.PodGroupInfo,
	results map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (result *PodGroupAlgorithmResult, revertFns revertFns) {
	podGroupCycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// CPG requires TopologyAwareWorkloadScheduling feature to be enabled
	return sched.compositePodGroupSchedulingPlacementAlgorithm(podGroupCycleCtx, schedFwk, podGroupCycleState, root, podGroupInfo, results)
}

// compositePodGroupSchedulingPlacementAlgorithm tries several different combinations for scheduling the child pod groups and selects the best one.
// First it runs placement generator plugins to create a list of placements.
// Placement is a set of nodes that will be considered when scheduling a pod group.
// Then for each placement it tries to schedule the pod group through compositePodGroupSchedulingDefaultAlgorithm.
// Finally, it runs placement scorer plugins to select the best placement.
//
// UPSTREAM-DIFF: adapted from Scheduler.compositePodGroupSchedulingPlacementAlgorithm.
func (sched *Scheduler) compositePodGroupSchedulingPlacementAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	root *framework.PodGroupInfo,
	podGroupInfo *framework.PodGroupInfo,
	results map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (finalResult *PodGroupAlgorithmResult, revertFns revertFns) {
	defer func() {
		results[getEntityKey(podGroupInfo)] = finalResult
	}()
	logger := klog.FromContext(ctx)
	allNodes, err := sched.nodeInfoSnapshot.ListNodesInPlacement()
	if err != nil {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       fwk.AsStatus(fmt.Errorf("failed to list node infos: %w", err)),
		}, nil
	}

	// For now, always record plugin metrics until we understand its impact on performance.
	podGroupCycleState.SetRecordPluginMetrics(true)
	placements, status := schedFwk.RunPlacementGeneratePlugins(ctx, podGroupCycleState, podGroupInfo, allNodes)
	if !status.IsSuccess() {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       status,
		}, nil
	}

	var anyResultSubtree map[fwk.EntityKey]*PodGroupAlgorithmResult
	successfulResults := make(map[*fwk.Placement]map[fwk.EntityKey]*PodGroupAlgorithmResult)

	parentPlacement := sched.nodeInfoSnapshot.GetPlacement()
	defer func() {
		sched.nodeInfoSnapshot.ForgetPlacement()
		err := sched.nodeInfoSnapshot.AssumePlacement(parentPlacement)
		if err != nil {
			finalResult.Status = fwk.AsStatus(fmt.Errorf("failed to restore parent pod group placement: %w", err))
			revertFns.revert()
		}
	}()

	for _, placement := range placements {
		logger.V(4).Info("Assuming placement in snapshot", "placement", placement.Name)
		err := sched.nodeInfoSnapshot.AssumePlacement(placement)
		if err != nil {
			return &PodGroupAlgorithmResult{
				PodGroupInfo: podGroupInfo,
				Status:       fwk.AsStatus(fmt.Errorf("failed to assume pod group placement: %w", err)),
			}, nil
		}
		placementCycleState := framework.NewCycleState()
		placementCycleState.SetPodGroupSchedulingCycle(podGroupCycleState)
		subtreeResult := map[fwk.EntityKey]*PodGroupAlgorithmResult{}
		result, placementRevertFns := sched.compositePodGroupSchedulingDefaultAlgorithm(ctx, schedFwk, placementCycleState, root, podGroupInfo, subtreeResult)
		placementRevertFns.revert()

		if result.Status.IsError() {
			return result, nil
		}

		if anyResultSubtree == nil {
			anyResultSubtree = subtreeResult
		}

		if result.Status.IsSuccess() {
			successfulResults[placement] = subtreeResult
		}
	}

	if len(successfulResults) == 0 {
		// We need to send events and set the status for pods in case all simulations were infeasible.
		// The selection of which simulation we report is arbitrary for now, but may change in the future.
		anyResultRoot := anyResultSubtree[getEntityKey(podGroupInfo)]
		anyResultRoot.Status = fwk.NewStatus(fwk.Unschedulable, fmt.Sprintf("0/%d placements are available, first placement status: %v", len(placements), anyResultRoot.Status.AsError())).WithError(errPodGroupUnschedulable)
		// It is critical to copy the entire anyResultSubtree into results.
		// If omitted, the pod results are reconstructed later using the generic parent error
		// (errPodGroupUnschedulable) rather than their original *framework.FitError.
		// Losing the FitError means we lose the UnschedulablePlugins for each pod,
		// which breaks the QueueingHints.
		maps.Copy(results, anyResultSubtree)
		return anyResultRoot, nil
	}

	bestPlacement, status := sched.findBestCompositePodGroupPlacement(ctx, schedFwk, podGroupCycleState, podGroupInfo, successfulResults)
	if !status.IsSuccess() {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       status,
		}, nil
	}

	bestResult := successfulResults[bestPlacement]

	revertFns, err = sched.assumeSubtreeWithRevert(ctx, schedFwk, podGroupInfo, bestResult)
	if err != nil {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       fwk.AsStatus(fmt.Errorf("failed to assume the subtree: %w", err)),
		}, nil
	}
	maps.Copy(results, bestResult)

	return bestResult[getEntityKey(podGroupInfo)], revertFns
}

// compositePodGroupSchedulingDefaultAlgorithm schedules a composite pod group by recursively scheduling
// its children. It uses PlacementFeasible plugins to verify if the composite group constraints
// remain satisfiable at each step of the recursion, aborting and reverting early if they cannot be met.
// The returned revertFns propagates revert functions from all child pod group evaluations up to the root level.
//
// UPSTREAM-DIFF: adapted from Scheduler.compositePodGroupSchedulingDefaultAlgorithm.
func (sched *Scheduler) compositePodGroupSchedulingDefaultAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	placementCycleState *framework.CycleState,
	root *framework.PodGroupInfo,
	podGroupInfo *framework.PodGroupInfo,
	results map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (result *PodGroupAlgorithmResult, revertFns revertFns) {
	logger := klog.FromContext(ctx)
	defer func() {
		results[getEntityKey(podGroupInfo)] = result
		if result.Status.IsSuccess() {
			logger.V(5).Info("Composite podgroup scheduling algorithm succeeded", "compositePodGroup", klog.KObj(podGroupInfo))
		} else {
			logger.V(5).Info("Composite podgroup scheduling algorithm failed", "compositePodGroup", klog.KObj(podGroupInfo), "status", result.Status)
			revertFns.revert()
			result.AnyScheduled = false
		}
	}()

	placementProgress := framework.PlacementProgress{
		Remaining: len(podGroupInfo.Children),
	}
	placementFeasibleStatus := schedFwk.RunPlacementFeasiblePlugins(ctx, placementCycleState, podGroupInfo, placementProgress)

	if placementFeasibleStatus.Code() == fwk.Unschedulable || placementFeasibleStatus.Code() == fwk.Error {
		return &PodGroupAlgorithmResult{
			PodGroupInfo:        podGroupInfo,
			Status:              placementFeasibleStatus,
			PlacementCycleState: placementCycleState,
		}, revertFns
	}
	anyScheduled := false
	for _, childPGInfo := range podGroupInfo.GetChildGroups() {
		childPodGroupState := framework.NewCycleState()
		childPodGroupState.SetPlacementCycleState(placementCycleState)
		childResult, childRevertFns := sched.podGroupSchedulingRecursiveAlgorithm(ctx, schedFwk, childPodGroupState, root, childPGInfo, results)
		if childResult.Status.IsError() {
			return &PodGroupAlgorithmResult{
				PodGroupInfo:        podGroupInfo,
				Status:              fwk.AsStatus(fmt.Errorf("composite pod group evaluation failed due to child error: %w", childResult.Status.AsError())),
				PlacementCycleState: placementCycleState,
			}, revertFns
		}
		anyScheduled = anyScheduled || childResult.AnyScheduled
		revertFns.append(childRevertFns)
		placementProgress.Remaining--
		if childResult.Status.IsSuccess() {
			placementProgress.Scheduled++
		}
		placementFeasibleStatus = schedFwk.RunPlacementFeasiblePlugins(ctx, placementCycleState, podGroupInfo, placementProgress)
		if placementFeasibleStatus.Code() == fwk.Unschedulable || placementFeasibleStatus.Code() == fwk.Error {
			break
		}
	}

	if placementFeasibleStatus.IsWait() {
		placementFeasibleStatus = fwk.NewStatus(fwk.Unschedulable, placementFeasibleStatus.Reasons()...).WithError(errPodGroupUnschedulable)
	}
	return &PodGroupAlgorithmResult{
		PodGroupInfo:        podGroupInfo,
		Status:              placementFeasibleStatus,
		PlacementCycleState: placementCycleState,
		AnyScheduled:        anyScheduled,
	}, revertFns
}

// PodGroupSchedulingAlgorithm attempts to schedule pods in the pod group according to the policy and constraints and returns the scheduling result for all evaluated pods in the pod group, not necessarily all pods in the pod group.
// The returned revertFns accumulates revert functions for all scheduled pods, allowing the caller to rollback tentative reservations if the pod group scheduling cycle fails.
//
// UPSTREAM-DIFF: adapted from Scheduler.podGroupSchedulingAlgorithm.
func (sched *Scheduler) podGroupSchedulingAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
) (*PodGroupAlgorithmResult, revertFns) {
	podGroupCycleCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	if utilfeature.DefaultFeatureGate.Enabled(features.TopologyAwareWorkloadScheduling) {
		return sched.podGroupSchedulingPlacementAlgorithm(podGroupCycleCtx, schedFwk, podGroupCycleState, podGroupInfo)
	}

	// The non-TAS default algorithm does not evaluate placement candidates, but it
	// still runs in a single implicit placement context so placement-scoped
	// extension points can use the same state plumbing as TAS.
	placementCycleState := framework.NewCycleState()
	placementCycleState.SetPodGroupSchedulingCycle(podGroupCycleState)
	return sched.podGroupSchedulingDefaultAlgorithm(podGroupCycleCtx, schedFwk, placementCycleState, podGroupInfo)
}

// podGroupSchedulingPlacementAlgorithm tries several different combinations for scheduling the pod group and selects the best one.
// First it runs placement generator plugins to create a list of placements.
// Placement is a set of nodes that will be considered when scheduling a pod group.
// Then for each placement it tries to schedule the pod group through podGroupSchedulingDefaultAlgorithm.
// Finally, it runs placement scorer plugins to select the best placement.
//
// UPSTREAM-DIFF: adapted from Scheduler.podGroupSchedulingPlacementAlgorithm.
// Uses ListNodesInPlacement to respect snapshot placement boundaries.
func (sched *Scheduler) podGroupSchedulingPlacementAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
) (finalResult *PodGroupAlgorithmResult, revertFns revertFns) {
	logger := klog.FromContext(ctx)
	allNodes, err := sched.nodeInfoSnapshot.ListNodesInPlacement()
	if err != nil {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       fwk.AsStatus(fmt.Errorf("failed to list node infos: %w", err)),
		}, nil
	}

	// For now, always record plugin metrics until we understand its impact on performance.
	podGroupCycleState.SetRecordPluginMetrics(true)
	placements, status := schedFwk.RunPlacementGeneratePlugins(ctx, podGroupCycleState, podGroupInfo, allNodes)
	if !status.IsSuccess() {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       status,
		}, nil
	}
	metrics.RecordGeneratedPlacements(schedFwk.ProfileName(), len(placements))

	var anyResult *PodGroupAlgorithmResult
	successfulResults := make(map[*fwk.Placement]*PodGroupAlgorithmResult)

	parentPlacement := sched.nodeInfoSnapshot.GetPlacement()
	defer func() {
		sched.nodeInfoSnapshot.ForgetPlacement()
		err := sched.nodeInfoSnapshot.AssumePlacement(parentPlacement)
		if err != nil {
			finalResult.Status = fwk.AsStatus(fmt.Errorf("failed to restore parent pod group placement: %w", err))
			revertFns.revert()
		}
	}()

	for _, placement := range placements {
		logger.V(4).Info("Assuming placement in snapshot", "placement", placement.Name)
		evaluationStart := time.Now()
		err := sched.nodeInfoSnapshot.AssumePlacement(placement)
		if err != nil {
			return &PodGroupAlgorithmResult{
				PodGroupInfo: podGroupInfo,
				Status:       fwk.AsStatus(fmt.Errorf("failed to assume pod group placement: %w", err)),
			}, nil
		}

		placementCycleState := framework.NewCycleState()
		placementCycleState.SetPodGroupSchedulingCycle(podGroupCycleState)
		result, placementRevertFns := sched.podGroupSchedulingDefaultAlgorithm(ctx, schedFwk, placementCycleState, podGroupInfo)
		placementRevertFns.revert()

		if result.Status.IsError() {
			return result, nil
		}

		if anyResult == nil {
			anyResult = result
		}

		// Errors are excluded by the early return above since they are internal
		// faults, not feasibility results.
		evaluationResult := metrics.InfeasibleResult
		if result.Status.IsSuccess() {
			evaluationResult = metrics.FeasibleResult
			successfulResults[placement] = result
		}
		metrics.ObservePlacementEvaluation(evaluationResult, schedFwk.ProfileName(), metrics.SinceInSeconds(evaluationStart))
	}

	if len(successfulResults) == 0 {
		// We need to send events and set the status for pods in case all simulations were infeasible.
		// The selection of which simulation we report is arbitrary for now, but may change in the future.
		anyResult.Status = fwk.NewStatus(fwk.Unschedulable, fmt.Sprintf("0/%d placements are available, first placement status: %v", len(placements), anyResult.Status.AsError())).WithError(errPodGroupUnschedulable)
		return anyResult, nil
	}

	bestPlacement, status := sched.findBestPodGroupPlacement(ctx, schedFwk, podGroupCycleState, podGroupInfo, successfulResults)
	if !status.IsSuccess() {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       status,
		}, nil
	}
	bestResult := successfulResults[bestPlacement]

	// UPSTREAM-DIFF: always re-assume the winning placement via assumeSubtreeWithRevert
	// (upstream only does this when features.CompositePodGroup is enabled), because
	// RunRootSchedulingAlgorithm in scheduler-library leaves scheduled pods assumed in
	// the snapshot and returns revertFn to the caller.
	revertFns, err = sched.assumeSubtreeWithRevert(ctx, schedFwk, podGroupInfo, map[fwk.EntityKey]*PodGroupAlgorithmResult{getEntityKey(podGroupInfo): bestResult})
	if err != nil {
		return &PodGroupAlgorithmResult{
			PodGroupInfo: podGroupInfo,
			Status:       fwk.AsStatus(fmt.Errorf("failed to assume the subtree: %w", err)),
		}, nil
	}

	return bestResult, revertFns
}

// assumeAndReserveWithRevert assumes and reserves the pod on the suggested host, returning a revert function to undo the reservation.
//
// UPSTREAM-DIFF: adapted from Scheduler.assumeAndReserveWithRevert.
func (sched *Scheduler) assumeAndReserveWithRevert(
	ctx context.Context,
	state fwk.CycleState,
	schedFramework framework.Framework,
	podInfo *framework.PodInfo,
	scheduleResult ScheduleResult,
) (*fwk.Status, func()) {
	assumedPodInfo, assumeStatus := sched.assumeAndReserve(ctx, state, schedFramework, podInfo, scheduleResult)
	if !assumeStatus.IsSuccess() {
		return assumeStatus, nil
	}
	return assumeStatus, func() {
		err := sched.unreserveAndForget(ctx, state, schedFramework, assumedPodInfo, scheduleResult.SuggestedHost)
		if err != nil {
			utilruntime.HandleErrorWithContext(ctx, err, "ForgetPod failed")
		}
	}
}

// podGroupSchedulingDefaultAlgorithm runs the default algorithm for scheduling a pod group.
// It tries to schedule each pod using standard filtering and scoring logic in a fixed order.
// If a pod requires preemption to be schedulable, subsequent pods in the algorithm
// treat that pod as already scheduled on that node with victims being already removed in memory.
// The returned revertFns accumulates revert functions for all scheduled pods, allowing the caller
// to rollback tentative reservations if the pod group scheduling cycle fails.
//
// UPSTREAM-DIFF: adapted from Scheduler.podGroupSchedulingDefaultAlgorithm.
// Uses the library's SchedulePod and tracks rollback functions via revertFns.
func (sched *Scheduler) podGroupSchedulingDefaultAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	placementCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
) (result *PodGroupAlgorithmResult, revertFns revertFns) {
	defer func() {
		if !result.Status.IsSuccess() {
			revertFns.revert()
			result.AnyScheduled = false
		}
	}()

	unscheduledPods := podGroupInfo.GetUnscheduledPods()

	result = &PodGroupAlgorithmResult{
		PodGroupInfo:        podGroupInfo,
		PodResults:          make([]AlgorithmResult, 0, len(unscheduledPods)),
		Status:              fwk.NewStatus(fwk.Unschedulable).WithError(errPodGroupUnschedulable),
		PlacementCycleState: placementCycleState,
	}

	logger := klog.FromContext(ctx)
	logger.V(5).Info("Running a pod group scheduling algorithm", "podGroup", klog.KObj(podGroupInfo), "unscheduledPodsCount", len(unscheduledPods))

	podGroupState, err := sched.nodeInfoSnapshot.PodGroupStates().Get(podGroupInfo.Namespace, podGroupInfo.Name)
	if err != nil {
		result.Status = fwk.AsStatus(fmt.Errorf("failed to get podGroup state for podGroup %s to compute gang feasibility: %w", klog.KObj(podGroupInfo), err))
		return result, nil
	}

	// Run PlacementFeasible plugins to check if the pod group can meet its constraints
	// before even attempting to schedule any pods.
	placementProgress := framework.PlacementProgress{
		Remaining: len(podGroupInfo.GetUnscheduledPods()),
		Scheduled: podGroupState.ScheduledPodsCount(),
	}
	placementFeasibleStatus := schedFwk.RunPlacementFeasiblePlugins(ctx, placementCycleState, podGroupInfo, placementProgress)
	result.Status = placementFeasibleStatus
	if placementFeasibleStatus.IsError() {
		// Do not evaluate any pods if PlacementFeasible plugins return error or unexpected status.
		result.Status = fwk.AsStatus(fmt.Errorf("failed to evaluate placement feasibility: %w", placementFeasibleStatus.AsError()))
		return result, nil
	}
	if placementFeasibleStatus.Code() == fwk.Unschedulable {
		// Unschedulable from PlacementFeasible plugins indicates that the pod group
		// cannot meet its constraints, even if we succeed in scheduling all the pods.
		// Exit early from the pod group algorithm.
		result.Status = fwk.NewStatus(fwk.Unschedulable, result.Status.Reasons()...).WithError(errPodGroupUnschedulable)
		return result, nil
	}

	anyScheduled := false
	for _, pod := range unscheduledPods {
		podResult, revertFn := sched.podGroupPodSchedulingAlgorithm(ctx, schedFwk, placementCycleState, podGroupInfo, pod)
		result.PodResults = append(result.PodResults, podResult)
		if revertFn != nil {
			revertFns.append([]func(){revertFn})
		}

		if !podResult.status.IsSuccess() && !podResult.status.IsRejected() {
			// When the algorithm returns error or unexpected status, stop evaluating the rest of the pods.
			result.Status = fwk.AsStatus(fmt.Errorf("failed to schedule other pod from a pod group: %w", podResult.status.AsError()))
			break
		}

		// Check if the pod group can still meet its constraints after scheduling the current pod.
		placementProgress.Remaining--
		if podResult.status.IsSuccess() {
			placementProgress.Scheduled++
		}
		placementFeasibleStatus := schedFwk.RunPlacementFeasiblePlugins(ctx, placementCycleState, podGroupInfo, placementProgress)
		if placementFeasibleStatus.IsError() {
			// Stop evaluating the rest of the pods if PlacementFeasible plugins return error or unexpected status.
			result.Status = fwk.AsStatus(fmt.Errorf("failed to evaluate placement feasibility: %w", placementFeasibleStatus.AsError()))
			break
		}

		result.Status = placementFeasibleStatus

		// Unschedulable from PlacementFeasible plugins indicates that the pod group
		// cannot meet its constraints regardless of how many more pods we check.
		// We can stop the scheduling loop early.
		if placementFeasibleStatus.Code() == fwk.Unschedulable {
			break
		}

		anyScheduled = anyScheduled || podResult.status.IsSuccess()
	}

	if result.Status.IsWait() {
		result.Status = fwk.NewStatus(fwk.Unschedulable, result.Status.Reasons()...).WithError(errPodGroupUnschedulable)
	}

	result.AnyScheduled = anyScheduled
	return result, revertFns
}

// podGroupPodSchedulingAlgorithm runs a scheduling algorithm for individual pod from a pod group.
// It returns the algorithm result together with the revert function.
// The returned revert function rolls back tentative node reservations for the pod if the overall
// pod group fails to schedule.
//
// UPSTREAM-DIFF: takes *v1.Pod instead of *framework.QueuedPodInfo, constructs a PendingPod,
// and delegates to SchedulePod.
func (sched *Scheduler) podGroupPodSchedulingAlgorithm(
	ctx context.Context,
	schedFwk framework.Framework,
	placementCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
	pod *v1.Pod,
) (AlgorithmResult, func()) {
	pInfo, _ := framework.NewPodInfo(pod)
	podCycleState := framework.NewCycleState()
	podCycleState.SetPodGroupSchedulingCycle(placementCycleState.GetPodGroupSchedulingCycle())
	podCycleState.SetPlacementCycleState(placementCycleState)

	pendingPod := &PendingPod{
		PodInfo:    pInfo,
		CycleState: podCycleState,
	}

	logger := klog.FromContext(ctx)
	logger = klog.LoggerWithValues(logger, "pod", klog.KObj(pod))
	ctx = klog.NewContext(ctx, logger)
	logger.V(4).Info("Attempting to schedule a pod belonging to a pod group", "podGroup", klog.KObj(podGroupInfo), "pod", klog.KObj(pod))

	return sched.SchedulePod(ctx, schedFwk, pendingPod)
}

// assumeSubtreeWithRevert runs assumeAndReserveWithRevert on all pods within the subtree.
// This is needed for placement-based algorithm, because after evaluating the results for all placements,
// the chosen result needs to be assumed for the other pods in the hierarchy to see the result.
//
// UPSTREAM-DIFF: adapted from Scheduler.assumeSubtreeWithRevert.
func (sched *Scheduler) assumeSubtreeWithRevert(
	ctx context.Context,
	schedFwk framework.Framework,
	pgi *framework.PodGroupInfo,
	results map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (_ revertFns, err error) {
	if results == nil {
		return nil, fmt.Errorf("results for the subtree are missing")
	}

	var revertFns revertFns
	defer func() {
		if err != nil {
			revertFns.revert()
		}
	}()

	for leafResult := range SuccessfulLeafResults(pgi, results) {
		for _, podResult := range leafResult.PodResults {
			if !podResult.status.IsSuccess() || podResult.GetNodeName() == "" {
				continue
			}
			status, revert := sched.assumeAndReserveWithRevert(ctx, podResult.cycleState, schedFwk, podResult.podInfo, podResult.scheduleResult)
			if revert != nil {
				revertFns = append(revertFns, revert)
			}
			if !status.IsSuccess() {
				return nil, status.AsError()
			}
		}
	}

	return revertFns, nil
}

// SuccessfulLeafResults walks the tree down to the successful leafs.
// A leaf is only deemed successful if its ancestors are also successful.
// If the results are missing for a given subtree, that subtree is skipped.
//
// UPSTREAM-DIFF: exported for use by pkg/upstreamsync/snapshot.
func SuccessfulLeafResults(root *framework.PodGroupInfo, results map[fwk.EntityKey]*PodGroupAlgorithmResult) iter.Seq[*PodGroupAlgorithmResult] {
	return func(yield func(*PodGroupAlgorithmResult) bool) {
		var walk func(pgi *framework.PodGroupInfo) bool
		walk = func(pgi *framework.PodGroupInfo) bool {
			result, ok := results[getEntityKey(pgi)]
			// Result may be missing because it may have been skipped due to PlacementFeasible status.
			// If the result for a given subtree is non-success (e.g. actualCount < minGroupCount), we treat all of its descendants as non-success with 0 pods scheduled.
			if !ok || !result.Status.IsSuccess() {
				return true
			}

			for _, child := range pgi.Children {
				if !walk(child) {
					return false
				}
			}

			if len(result.PodResults) > 0 {
				return yield(result)
			}

			return true
		}
		walk(root)
	}
}

// UPSTREAM-DIFF: adapted from Scheduler.findBestPodGroupPlacement.
func (sched *Scheduler) findBestPodGroupPlacement(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState fwk.PodGroupCycleState,
	podGroupInfo *framework.PodGroupInfo,
	successfulResults map[*fwk.Placement]*PodGroupAlgorithmResult,
) (*fwk.Placement, *fwk.Status) {
	if len(successfulResults) == 1 {
		for placement := range successfulResults {
			return placement, nil
		}
	}

	placementPodGroupAssignments, placementStates := makePodGroupAssignments(successfulResults)
	return sched.findBestPlacement(ctx, schedFwk, podGroupCycleState, podGroupInfo, placementPodGroupAssignments, placementStates)
}

// UPSTREAM-DIFF: adapted from Scheduler.findBestCompositePodGroupPlacement.
func (sched *Scheduler) findBestCompositePodGroupPlacement(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState fwk.PodGroupCycleState,
	podGroupInfo *framework.PodGroupInfo,
	successfulResults map[*fwk.Placement]map[fwk.EntityKey]*PodGroupAlgorithmResult,
) (*fwk.Placement, *fwk.Status) {
	if len(successfulResults) == 1 {
		for placement := range successfulResults {
			return placement, nil
		}
	}

	placementPodGroupAssignments, placementStates := makeCompositePodGroupAssignments(podGroupInfo, successfulResults)
	return sched.findBestPlacement(ctx, schedFwk, podGroupCycleState, podGroupInfo, placementPodGroupAssignments, placementStates)
}

// findBestPlacement uses PlacementScore plugins to determine the best placement based on the scheduling results.
//
// UPSTREAM-DIFF: adapted from Scheduler.findBestPlacement.
func (sched *Scheduler) findBestPlacement(
	ctx context.Context,
	schedFwk framework.Framework,
	podGroupCycleState fwk.PodGroupCycleState,
	podGroupInfo *framework.PodGroupInfo,
	placementPodGroupAssignments []*fwk.PodGroupAssignments,
	placementStates []fwk.PlacementCycleState,
) (*fwk.Placement, *fwk.Status) {
	scores, status := schedFwk.RunPlacementScorePlugins(ctx, podGroupCycleState, podGroupInfo, placementPodGroupAssignments, placementStates)
	if !status.IsSuccess() {
		return nil, status
	}

	for i := range scores {
		scores[i].Randomizer = rand.Int()
	}

	loggerVTen := klog.FromContext(ctx).V(10)
	if loggerVTen.Enabled() {
		for _, score := range scores {
			for _, pluginScore := range score.Scores {
				loggerVTen.Info("Plugin scored placement for podGroup", "podGroup", klog.KObj(podGroupInfo), "plugin", pluginScore.Name, "placement", score.Placement.Name, "score", pluginScore.Score)
			}
			loggerVTen.Info("Calculated placement's final score for podGroup", "podGroup", klog.KObj(podGroupInfo), "placement", score.Placement.Name, "score", score.TotalScore)
		}
	}

	bestScore := &scores[0]
	for _, score := range scores[1:] {
		if score.TotalScore > bestScore.TotalScore ||
			(score.TotalScore == bestScore.TotalScore && score.Randomizer > bestScore.Randomizer) {
			bestScore = &score
		}
	}
	return bestScore.Placement, nil
}

// makeProposedAssignments extracts ProposedAssignment entries for pods that were assigned a host.
//
// UPSTREAM-DIFF: none, copied verbatim.
func makeProposedAssignments(res *PodGroupAlgorithmResult) []fwk.ProposedAssignment {
	proposedAssignments := make([]fwk.ProposedAssignment, 0, len(res.PodResults))
	for i := range res.PodResults {
		if res.PodResults[i].status.IsSuccess() && res.PodResults[i].GetNodeName() != "" {
			proposedAssignments = append(proposedAssignments, &res.PodResults[i])
		}
	}
	return proposedAssignments
}

// makePodGroupAssignments converts scheduling results for PodGroup from candidate placements into the format
// required by PlacementScore plugins to score and select the best placement for the pod group.
//
// UPSTREAM-DIFF: adapted from makePodGroupAssignments.
func makePodGroupAssignments(successfulResults map[*fwk.Placement]*PodGroupAlgorithmResult) ([]*fwk.PodGroupAssignments, []fwk.PlacementCycleState) {
	placementAssignments := make([]*fwk.PodGroupAssignments, 0, len(successfulResults))
	placementStates := make([]fwk.PlacementCycleState, 0, len(successfulResults))
	for placement, res := range successfulResults {
		placementAssignments = append(placementAssignments, &fwk.PodGroupAssignments{
			Placement:           placement,
			ProposedAssignments: makeProposedAssignments(res),
		})
		placementStates = append(placementStates, res.PlacementCycleState)
	}
	return placementAssignments, placementStates
}

// makeCompositePodGroupAssignments converts scheduling results for CompositePodGroup from candidate placements into the format
// required by PlacementScore plugins to score and select the best placement for the composite pod group.
//
// UPSTREAM-DIFF: none, copied verbatim.
func makeCompositePodGroupAssignments(
	pgi *framework.PodGroupInfo,
	successfulResults map[*fwk.Placement]map[fwk.EntityKey]*PodGroupAlgorithmResult,
) ([]*fwk.PodGroupAssignments, []fwk.PlacementCycleState) {
	placementPodGroupAssignments := make([]*fwk.PodGroupAssignments, 0, len(successfulResults))
	placementStates := make([]fwk.PlacementCycleState, 0, len(successfulResults))
	for placement, subtreeResults := range successfulResults {
		var combinedProposedAssignments []fwk.ProposedAssignment
		for result := range SuccessfulLeafResults(pgi, subtreeResults) {
			combinedProposedAssignments = append(combinedProposedAssignments, makeProposedAssignments(result)...)
		}
		placementPodGroupAssignments = append(placementPodGroupAssignments, &fwk.PodGroupAssignments{
			Placement:           placement,
			ProposedAssignments: combinedProposedAssignments,
		})
		placementStates = append(placementStates, subtreeResults[getEntityKey(pgi)].PlacementCycleState)
	}
	return placementPodGroupAssignments, placementStates
}

func getEntityKey(pgi *framework.PodGroupInfo) fwk.EntityKey {
	if pgi.Type == fwk.CompositePodGroupKeyType {
		return fwk.CompositePodGroupKey(pgi.Namespace, pgi.Name)
	}
	return fwk.PodGroupKey(pgi.Namespace, pgi.Name)
}
