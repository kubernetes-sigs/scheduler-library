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
	"errors"
	"fmt"
	"iter"
	"math"
	"reflect"
	"slices"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-library/pkg/preemption"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
	upstreampreemption "sigs.k8s.io/scheduler-library/pkg/upstreamsync/preemption"
)

// ClusterSnapshot wraps a scheduler snapshot and its associated frameworks.
// The ClusterSnapshot of a ClusterState (state.ClusterState.GetAssociatedSnapshot) shares its
// underlying cache.Snapshot with that ClusterState. ClusterState.SyncSnapshot updates that shared
// snapshot in-place and reverts the mutations made through the ClusterSnapshot since the previous
// sync.
// A ClusterSnapshot is not safe for concurrent use.
type ClusterSnapshot struct {
	// profiles holds the scheduling framework per scheduler name. All of them share
	// schedulerSnapshot as their SnapshotSharedLister, so the plugins always see the mutations
	// performed here.
	profiles *upstreamsync.ProfileMap
	// schedulerSnapshot is the upstream snapshot holding the actual node and pod data.
	schedulerSnapshot *cache.Snapshot
	// undoLog records how to undo every mutation applied to schedulerSnapshot, so that a dry run
	// or a reverted transaction can restore the state it started from.
	undoLog undoLog
	// transactionInProgress guards against nested transactions and tells the mutating methods to
	// leave the undo log alone, as the enclosing transaction owns it.
	transactionInProgress bool
	// stateVersionForPreemption is bumped whenever a mutation makes the outstanding Unpreemption
	// handles unusable, i.e. whenever the state they would be restoring into is no longer the one
	// they were taken from. Unpreempt compares it with the value recorded in the handle.
	stateVersionForPreemption uint64
}

// undoLog is a stack of the operations reverting the mutations applied to the snapshot, most
// recent last. Every mutation pushes its revert function, and rolling back means popping and
// running them until the recorded state version is reached again.
type undoLog struct {
	// undoOperations are the revert functions, in the order their mutations were applied.
	undoOperations []func()
	// stateVersion is incremented by every registered operation and decremented by every undone
	// one. A caller records it before mutating and passes it to restoreState afterwards to undo
	// exactly its own mutations.
	stateVersion uint64
}

// registerOperation pushes the revert function of a mutation that has just been applied.
// A nil undoOperation is ignored, so that callers can pass the result of an operation that
// did not change anything.
func (ul *undoLog) registerOperation(undoOperation func()) {
	if undoOperation != nil {
		ul.undoOperations = append(ul.undoOperations, undoOperation)
		ul.stateVersion++
	}
}

// restoreState undoes the operations registered after the given state version was observed,
// in the reverse order of their registration.
func (ul *undoLog) restoreState(stateVersion uint64) {
	for ul.stateVersion != stateVersion {
		ul.undo()
	}
}

// undo pops the most recently registered operation and runs it.
func (ul *undoLog) undo() {
	ops := ul.undoOperations
	ops, undoOp := ops[:len(ops)-1], ops[len(ops)-1]
	ul.undoOperations = ops
	undoOp()
	ul.stateVersion--
}

// New creates a new ClusterSnapshot stub wrapping the provided scheduler snapshot and frameworks.
//
// Consumers should obtain a ClusterSnapshot from simulator.SchedulingSimulator instead, either via
// NewClusterSnapshot or via NewClusterState followed by state.ClusterState.GetAssociatedSnapshot: those build
// the full plugin chain out of the KubeSchedulerConfiguration and initialize the scheduler metrics,
// which this constructor expects to have been done already.
func New(s *cache.Snapshot, profiles *upstreamsync.ProfileMap) *ClusterSnapshot {
	return &ClusterSnapshot{
		profiles:          profiles,
		schedulerSnapshot: s,
	}
}

// ResetMutations restores the snapshot to its state prior to any mutations,
// executing all accumulated undo operations in reverse order.
func (s *ClusterSnapshot) ResetMutations() error {
	if s.transactionInProgress {
		return fmt.Errorf("transaction is in progress, cannot reset mutations")
	}
	if s.undoLog.stateVersion > 0 {
		s.undoLog.restoreState(0)
		s.stateVersionForPreemption++
	}
	return nil
}

// Transaction executes the provided function within a transaction.
// It rolls back operations if the function returns Revert or an error.
// Only a single active transaction is supported at any given time;
// attempting to start a nested transaction will return an error.
// Committed operations or operations made outside of transaction scope
// can only be reverted by [ClusterSnapshot.ResetMutations].
func (s *ClusterSnapshot) Transaction(ctx context.Context, transactionFn func() (TransactionResult, error)) error {
	if s.transactionInProgress {
		return fmt.Errorf("a transaction is already in progress")
	}

	s.transactionInProgress = true
	defer func() { s.transactionInProgress = false }()

	initialStateVersion := s.undoLog.stateVersion
	initialStateVersionForPreemption := s.stateVersionForPreemption
	s.stateVersionForPreemption++

	result, err := transactionFn()

	if err != nil || result == Revert {
		s.undoLog.restoreState(initialStateVersion)
		s.stateVersionForPreemption = initialStateVersionForPreemption
	} else {
		// invalidate preemptions done within the transaction
		s.stateVersionForPreemption++
	}

	if err != nil {
		return fmt.Errorf("transaction failed: %w", err)
	}
	return nil
}

// CanSchedulePod checks feasibility of a single pod on the specified nodes by running
// PreFilter and Filter plugins, leaving the snapshot untouched. See FeasibilityResult for
// what it reports; its CycleState is what state.ClusterState.AssumeAndReserve needs to reserve
// one of the feasible nodes.
func (s *ClusterSnapshot) CanSchedulePod(ctx context.Context, pod *v1.Pod, placement *fwk.Placement) (FeasibilityResult, error) {
	if placement == nil || len(placement.Nodes) == 0 {
		return FeasibilityResult{}, nil
	}
	schedFramework, err := s.profiles.FrameworkForPod(pod)
	if err != nil {
		return FeasibilityResult{}, fmt.Errorf("failed to get framework: %w", err)
	}
	state := framework.NewCycleState()
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		return FeasibilityResult{}, fmt.Errorf("failed to create pod info: %w", err)
	}
	pendingPod := &upstreamsync.PendingPod{
		PodInfo:    podInfo,
		CycleState: state,
	}

	sched := upstreamsync.NewScheduler(s.schedulerSnapshot, 0, 0, math.MaxInt32, nil)
	err = s.schedulerSnapshot.AssumePlacement(placement)
	if err != nil {
		return FeasibilityResult{}, fmt.Errorf("failed to assume placement: %w", err)
	}
	defer s.schedulerSnapshot.ForgetPlacement()
	nodes, diag, _, err := sched.FindAllNodesThatFitPod(ctx, schedFramework, pendingPod)
	if err != nil {
		return FeasibilityResult{Diagnosis: &diag}, fmt.Errorf("failed to find nodes that fit pod: %w", err)
	}
	feasibleNodes := make([]string, 0, len(nodes))
	for _, node := range nodes {
		feasibleNodes = append(feasibleNodes, node.Node().Name)
	}

	return FeasibilityResult{FeasibleNodeNames: feasibleNodes, CycleState: state, Diagnosis: &diag}, nil
}

func schedulingResult(algRes *upstreamsync.AlgorithmResult) SchedulingResult {
	return SchedulingResult{
		Pod:              algRes.GetPod(),
		Status:           algRes.GetStatus(),
		SelectedNodeName: algRes.GetNodeName(),
		CycleState:       algRes.GetCycleState(),
	}
}

// SchedulePods schedules the given pods onto the specified placement using PreFilter and Filter plugins.
// StopOnFailure controls whether the first unschedulable pod stops the loop. Note that
// All unexpected execution errors always propagate immediately regardless of StopOnFailure, as they
// indicate a programming error rather than a scheduling failure.
// The pods passed in are left untouched. Each result carries the library's own copy of the pod the
// attempt was made for, with Spec.NodeName set to the selected node when it was scheduled; that
// copy is what PreemptPods takes to remove the pod again. On a pod that was not scheduled
// Spec.NodeName is left as it came in, so it is empty unless the caller already set one.
func (s *ClusterSnapshot) SchedulePods(ctx context.Context, pods []*v1.Pod, placement *fwk.Placement, opts SchedulePodsOptions) ([]SchedulingResult, error) {
	return s.schedulePods(ctx, ownedCopies(pods), placement, opts)
}

// ownedCopies yields a copy of every pod, so that the simulation records the placement it made on a
// pod of its own rather than on one the caller passed in and still owns. The pods generated from a
// template need no such copy, as nothing outside the library holds them.
// The pods are copied one at a time rather than the whole slice up front, so a run that stops early
// never copies the pods it does not attempt.
func ownedCopies(pods []*v1.Pod) iter.Seq[*v1.Pod] {
	return func(yield func(*v1.Pod) bool) {
		for _, pod := range pods {
			if !yield(pod.DeepCopy()) {
				return
			}
		}
	}
}

// SchedulePodsByTemplate attempts to schedule as many pods matching the template as possible.
// It assumes nodes in the placement are feasible and moves to the next node only if the pod is unschedulable on the current node.
func (s *ClusterSnapshot) SchedulePodsByTemplate(ctx context.Context, template *v1.PodTemplateSpec, placement *fwk.Placement, maxPods int, opts SchedulePodsByTemplateOptions) ([]SchedulingResult, error) {
	if maxPods <= 0 {
		return nil, nil
	}

	podIterator := func(yield func(*v1.Pod) bool) {
		for i := range maxPods {
			pod := createPodFromTemplate(template, i)
			if !yield(pod) {
				return
			}
		}
	}

	scheduleOptions := SchedulePodsOptions{
		CommonSchedulingOptions: opts.CommonSchedulingOptions,
		StopOnFailure:           true,
	}

	return s.schedulePods(ctx, podIterator, placement, scheduleOptions)
}

func (s *ClusterSnapshot) schedulePods(ctx context.Context, pods iter.Seq[*v1.Pod], placement *fwk.Placement, opts SchedulePodsOptions) (_ []SchedulingResult, err error) {
	if placement == nil || len(placement.Nodes) == 0 {
		return nil, nil
	}

	initialStateVersion := s.undoLog.stateVersion

	defer func() {
		if err != nil || opts.DryRun {
			s.undoLog.restoreState(initialStateVersion)
		}
		if initialStateVersion != s.undoLog.stateVersion {
			s.stateVersionForPreemption++
		}
	}()

	result := make([]SchedulingResult, 0)

	currentCycle := int64(0)

	err = s.schedulerSnapshot.AssumePlacement(placement)
	if err != nil {
		return nil, fmt.Errorf("error assuming placement: %w", err)
	}
	defer s.schedulerSnapshot.ForgetPlacement()
	for pod := range pods {
		sched := upstreamsync.NewScheduler(s.schedulerSnapshot, currentCycle, 0, 1, nil)

		res, revertFn, err := scheduleOnePod(ctx, s.profiles, sched, pod)

		if err != nil {
			return result, err
		}

		if res.GetStatus().IsSuccess() {
			// The pod object is a copy made within the simulation library. It is not modified by
			// scheduleOnePod, but it is returned in the result object. To make the result placement
			// visible to the caller, pod.Spec.NodeName needs to be set.
			pod.Spec.NodeName = res.GetNodeName()
		}

		if revertFn != nil {
			s.undoLog.registerOperation(revertFn)
		}
		result = append(result, schedulingResult(res))

		if !res.GetStatus().IsSuccess() {
			if opts.StopOnFailure {
				return result, nil
			}
		}

		currentCycle++
	}

	return result, nil
}

// MakePlacement creates a framework.Placement containing NodeInfo structures for each candidate node name.
func (s *ClusterSnapshot) MakePlacement(candidateNodeNames []string) (*fwk.Placement, error) {
	nodes := make([]fwk.NodeInfo, 0, len(candidateNodeNames))
	for _, name := range candidateNodeNames {
		ni, err := s.schedulerSnapshot.NodeInfos().Get(name)
		if err != nil {
			return nil, fmt.Errorf("error getting %s from snapshot: %w", name, err)
		}
		nodes = append(nodes, ni)
	}
	return &fwk.Placement{Nodes: nodes}, nil
}

// PreemptPods removes pods from the snapshot.
// It supports transaction rollbacks if called inside a transaction.
// If any pod fails to be preempted, all previously preempted pods in this call
// are automatically restored and an error is returned.
func (s *ClusterSnapshot) PreemptPods(ctx context.Context, pods []*v1.Pod) (_ *Unpreemption, err error) {
	// Validate all pods before making any changes.
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			return nil, fmt.Errorf("pod %s has no node name", klog.KObj(pod))
		}
	}

	initialStateVersion := s.undoLog.stateVersion

	defer func() {
		if err != nil {
			s.undoLog.restoreState(initialStateVersion)
		}
	}()

	mutatingSnapshot := upstreamsync.NewMutatingSnapshot(s.schedulerSnapshot)

	unpreemptFns := []func() error{}

	for _, pod := range pods {
		revertFn, err := removePodFromNode(ctx, mutatingSnapshot, pod)
		if err != nil {
			return nil, fmt.Errorf("failed to unreserve and forget pod %s: %w", klog.KObj(pod), err)
		}
		s.undoLog.registerOperation(revertFn)
		// Putting the pod back is a snapshot mutation like any other, so it goes through
		// addPodToNode and registers its own revert function, undoing it re-preempts the pod.
		unpreemptFns = append(unpreemptFns, func() error {
			repreemptFn, err := addPodToNode(ctx, mutatingSnapshot, pod, pod.Spec.NodeName)
			if err != nil {
				return fmt.Errorf("failed to unpreempt pod %s: %w", klog.KObj(pod), err)
			}
			s.undoLog.registerOperation(repreemptFn)
			return nil
		})
	}

	unpreemptFn := func() error {
		var errs []error
		for _, unpreempt := range slices.Backward(unpreemptFns) {
			// Keep going on failure, so that as many pods as possible are put back.
			if err := unpreempt(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	return &Unpreemption{
		pods:                   pods,
		revertFn:               unpreemptFn,
		validPreemptionVersion: s.stateVersionForPreemption,
	}, nil
}

// Unpreempt undos the preemption done by the PreemptPods.
// The handle is consumed even if putting some of the pods back fails, in which case the pods that
// were restored are still registered in the undo log and are rolled back with the transaction.
func (s *ClusterSnapshot) Unpreempt(u *Unpreemption) ([]*v1.Pod, error) {
	if u == nil {
		return nil, fmt.Errorf("preemption handle is nil")
	}
	if s.stateVersionForPreemption != u.validPreemptionVersion {
		return nil, fmt.Errorf("preemption handle is invalid: snapshot has been permanently mutated since preemption")
	}
	if u.reverted {
		return nil, fmt.Errorf("preemption handle is invalid: already unpreempted")
	}

	defer func() {
		u.reverted = true
	}()

	err := u.revertFn()
	if err != nil {
		return nil, err
	}

	return u.pods, nil
}

// ScheduleWorkload schedules the given pods belonging to the same hierarchy using the workload-aware scheduling algorithm.
// If the pods do not belong to the same hierarchy, it returns an error status.
// The order of WorkloadSchedulingResult.PodResults is not guaranteed to match the order of input pods.
//
// ScheduleWorkload evaluates up to three passes depending on opts.CommittedVictims and opts.PotentialVictims:
//  1. Fit without preemption: attempts scheduling with nodes occupied by opts.CommittedVictims filtered out
//     and opts.CommittedVictims temporarily excluded from opts.PreemptionFilter (if set). On success, returns
//     Status: Success, WaitingForPreemption: false, and empty PreemptionVictims.
//  2. Fit waiting for CommittedVictims: if pass 1 returns Unschedulable and opts.CommittedVictims is non-empty,
//     retries scheduling across all nodes with opts.CommittedVictims active in opts.PreemptionFilter. On success,
//     returns Status: Unschedulable, WaitingForPreemption: true, empty PreemptionVictims, and the proposed PodResults.
//  3. Fit via new preemption: if still Unschedulable and opts.PotentialVictims is non-nil, evaluates preemption over
//     opts.PotentialVictims. On success, returns Status: Unschedulable, WaitingForPreemption: true, the newly required
//     victims in PreemptionVictims (excluding opts.CommittedVictims), and the proposed PodResults.
//
// Snapshot rollback asymmetry:
//   - Pass 1 (Status: Success) keeps the scheduled pods assumed in the snapshot and updates each scheduled pod's
//     Spec.NodeName unless opts.DryRun is true.
//   - Pass 2, Pass 3, and all failure outcomes return a non-Success Status and roll back every snapshot mutation made
//     during the call (leaving input pods' Spec.NodeName untouched).
//
// Incremental multi-workload simulation:
// When simulating workload w1 followed by workload w2 and committing w1's preemption (WaitingForPreemption: true),
// the caller must:
//   - remove w1's PreemptionVictims from the snapshot via PreemptPods and append them to the
//     CommittedVictims slice passed to w2;
//   - explicitly place w1's scheduled pods onto their selected nodes in the snapshot before
//     scheduling w2 (for example by calling SchedulePods with MakePlacement([]string{pRes.SelectedNodeName})
//     for each entry in w1's PodResults), because ScheduleWorkload does not update the snapshot when
//     preemption is needed even if DryRun is false;
//   - ensure opts.PreemptionFilter (if specified) considers CommittedVictims as initially preempted
//     and allows them to be temporarily unpreempted ([preemption.PreemptionFilter.UnpreemptVictim])
//     and re-preempted ([preemption.PreemptionFilter.PreemptVictim]).
//
// All PodGroup and CompositePodGroup objects in the hierarchy must exist in the snapshot before calling this method.
// Callers can register virtual groups (groups not present in the cluster) with AddPodGroup and AddCompositePodGroup.
//
// Callers are expected to pass pods that are not yet tracked in the snapshot's pod group states, because
// ScheduleWorkload adds each input pod to its pod group state as an unscheduled member. If an input pod is already
// tracked (for example, added through Cache.AddPodGroupMember or passed to SchedulingSimulator.NewClusterSnapshot),
// ScheduleWorkload returns an error status.
//
// Callers must enable the GenericWorkload feature gate (plus TopologyAwareWorkloadScheduling and
// CompositePodGroup when using topology-aware scheduling or composite hierarchies). Custom scheduler
// profiles should keep the GangScheduling plugin enabled (via MultiPoint or Permit, which registers its
// PlacementFeasible extension point) for gang and basic quorum enforcement.
func (s *ClusterSnapshot) ScheduleWorkload(ctx context.Context, pods []*v1.Pod, opts ScheduleWorkloadOptions) (result WorkloadSchedulingResult) {
	if len(pods) == 0 {
		return WorkloadSchedulingResult{Status: fwk.NewStatus(fwk.Success)}
	}

	initialStateVersion := s.undoLog.stateVersion
	var unscheduledPodsRevertFns map[types.UID]func()

	defer func() {
		if !result.Status.IsSuccess() || opts.DryRun {
			s.undoLog.restoreState(initialStateVersion)
		} else {
			for _, r := range result.PodResults {
				if r.SelectedNodeName != "" && r.Pod != nil {
					r.Pod.Spec.NodeName = r.SelectedNodeName
					delete(unscheduledPodsRevertFns, r.Pod.UID)
				}
			}
			for _, revertUnscheduledPod := range unscheduledPodsRevertFns {
				revertUnscheduledPod()
			}
		}
		if initialStateVersion != s.undoLog.stateVersion {
			s.stateVersionForPreemption++
		}
	}()

	podGroupInfo, err := buildPodGroupHierarchy(s.schedulerSnapshot, pods)
	if err != nil {
		return WorkloadSchedulingResult{Status: fwk.AsStatus(fmt.Errorf("failed to build pod group hierarchy: %w", err))}
	}

	schedFramework, err := s.profiles.FrameworkForPodGroup(podGroupInfo)
	if err != nil {
		return WorkloadSchedulingResult{Status: fwk.AsStatus(fmt.Errorf("failed to get framework for pod group: %w", err))}
	}

	unscheduledPodsRevertFns, err = s.addUnscheduledPodGroupMembers(ctx, pods)
	if err != nil {
		return WorkloadSchedulingResult{Status: fwk.AsStatus(err)}
	}

	// First, try scheduling without preemption with the nodes occupied by committed victims filtered out.
	// Fit here means the workload doesn't need to wait for preemption.
	sched := upstreamsync.NewScheduler(s.schedulerSnapshot, 0, 0, 1, nil)
	rootCycleState := framework.NewCycleState()
	err = preemption.SetCommittedVictimsInCycleState(rootCycleState, opts.CommittedVictims)
	if err != nil {
		return WorkloadSchedulingResult{Status: fwk.AsStatus(fmt.Errorf("failed to set committed victims: %w", err))}
	}

	// To correctly check `PreemptionFilter.MayFit` in the first pass, we need to pretend the committed victims are present in the cluster.
	if opts.PreemptionFilter != nil {
		for _, v := range slices.Backward(opts.CommittedVictims) {
			opts.PreemptionFilter.UnpreemptVictim(v)
		}
	}
	initialResult := s.scheduleWorkloadWithoutPreemption(ctx, sched, schedFramework, rootCycleState, podGroupInfo, opts)
	if opts.PreemptionFilter != nil {
		for _, v := range opts.CommittedVictims {
			opts.PreemptionFilter.PreemptVictim(v)
		}
	}
	if initialResult.Status.Code() != fwk.Unschedulable {
		return initialResult
	}

	// Second, try scheduling without preemption with all nodes available.
	// Fit here means the workload needs to wait for preemption of CommittedVictims.
	if len(opts.CommittedVictims) > 0 {
		rootCycleState = framework.NewCycleState()
		initialResult = s.scheduleWorkloadWithoutPreemption(ctx, sched, schedFramework, rootCycleState, podGroupInfo, opts)
		if initialResult.Status.IsSuccess() {
			initialResult.Status = fwk.NewStatus(fwk.Unschedulable, "waiting for preemption of committed victims")
			initialResult.WaitingForPreemption = true
			return initialResult
		}
		if initialResult.Status.Code() != fwk.Unschedulable {
			return initialResult
		}
	}

	if opts.PotentialVictims == nil {
		return initialResult
	}

	// Third, try scheduling with preemption of the provided potential victims.
	// Fit here means the workload needs to wait for new victims to be preempted.
	return s.workloadPreemption(ctx, sched, schedFramework, rootCycleState, podGroupInfo, initialResult, opts)
}

// addUnscheduledPodGroupMembers adds a copy of each pod to its pod group state as an unscheduled member.
// For each pod, a revert function is created and registered in the undo log, and returned so that
// ScheduleWorkload can remove only the pods that weren't scheduled.
func (s *ClusterSnapshot) addUnscheduledPodGroupMembers(ctx context.Context, pods []*v1.Pod) (map[types.UID]func(), error) {
	logger := klog.FromContext(ctx)
	unscheduledPodsRevertFns := make(map[types.UID]func(), len(pods))
	for _, pod := range pods {
		// Store a copy, since the pod group state keeps it by reference and the input pod changes on success.
		podCopy := pod.DeepCopy()
		podCopy.Spec.NodeName = ""
		if err := s.addPodGroupMember(podCopy); err != nil {
			return nil, fmt.Errorf("failed to add pod %s to its pod group state: %w", klog.KObj(pod), err)
		}

		// revertFn may run twice: from ScheduleWorkload for pods that weren't scheduled, and later from the
		// undo log (ResetMutations or a reverted Transaction). If the pod is already removed, the second call is a no-op.
		podRemoved := false
		revertFn := func() {
			if podRemoved {
				return
			}
			podRemoved = true
			if err := s.removePodGroupMember(podCopy); err != nil {
				logger.Error(err, "Failed to remove pod group member during state revert", "pod", klog.KObj(podCopy))
			}
		}
		unscheduledPodsRevertFns[podCopy.UID] = revertFn
		s.undoLog.registerOperation(revertFn)
	}
	return unscheduledPodsRevertFns, nil
}

func (s *ClusterSnapshot) scheduleWorkloadWithoutPreemption(
	ctx context.Context,
	sched *upstreamsync.Scheduler,
	schedFramework *upstreampreemption.PreemptionFramework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
	opts ScheduleWorkloadOptions,
) WorkloadSchedulingResult {
	if opts.PreemptionFilter != nil && !opts.PreemptionFilter.MayFit() {
		return WorkloadSchedulingResult{
			Status: fwk.NewStatus(fwk.Unschedulable, "preemption filter reported that workload cannot fit without preemption"),
		}
	}

	algResultsMap, revertFn := sched.RunRootSchedulingAlgorithm(ctx, schedFramework, podGroupCycleState, podGroupInfo)
	rootResult := algResultsMap[upstreamsync.GetEntityKey(podGroupInfo)]
	if rootResult.Status.IsError() {
		return WorkloadSchedulingResult{Status: rootResult.Status}
	}
	if !rootResult.Status.IsSuccess() {
		return WorkloadSchedulingResult{
			Status:     rootResult.Status,
			PodResults: buildFailedWorkloadPodResults(podGroupInfo, podGroupCycleState, algResultsMap),
		}
	}

	if revertFn != nil {
		s.undoLog.registerOperation(revertFn)
	}
	var podResults []SchedulingResult
	for res := range upstreamsync.SuccessfulLeafResults(podGroupInfo, algResultsMap) {
		for _, pRes := range res.PodResults {
			if pRes.GetStatus().IsSuccess() {
				podResults = append(podResults, SchedulingResult{
					Pod:              pRes.GetPod(),
					Status:           pRes.GetStatus(),
					SelectedNodeName: pRes.GetNodeName(),
					CycleState:       pRes.GetCycleState(),
				})
			}
		}
	}
	return WorkloadSchedulingResult{
		Status:     rootResult.Status,
		PodResults: podResults,
	}
}

func (s *ClusterSnapshot) workloadPreemption(
	ctx context.Context,
	sched *upstreamsync.Scheduler,
	schedFramework *upstreampreemption.PreemptionFramework,
	podGroupCycleState *framework.CycleState,
	podGroupInfo *framework.PodGroupInfo,
	initialResult WorkloadSchedulingResult,
	opts ScheduleWorkloadOptions,
) WorkloadSchedulingResult {
	baseSchedulingFunc := sched.BuildPodGroupSchedulingFunc(schedFramework, podGroupInfo)

	// Pull PotentialVictims and run PodGroup preemption with reprieval.
	assignments, preemptedVictims, status := preemption.EvaluateWorkloadPreemption(
		ctx,
		schedFramework,
		podGroupCycleState,
		podGroupInfo,
		baseSchedulingFunc,
		opts.WorkloadPreemptionOptions,
	)
	if status.IsError() {
		return WorkloadSchedulingResult{Status: status}
	}
	if !status.IsSuccess() {
		return initialResult
	}

	return WorkloadSchedulingResult{
		Status:               initialResult.Status,
		PodResults:           buildWorkloadPodResults(assignments),
		WaitingForPreemption: true,
		PreemptionVictims:    preemptedVictims,
	}
}

// buildFailedWorkloadPodResults completes pod group results across the hierarchy and traverses
// the hierarchy in DFS order to collect results for all pods with SelectedNodeName cleared.
func buildFailedWorkloadPodResults(
	root *framework.PodGroupInfo,
	podGroupCycleState *framework.CycleState,
	algResultsMap map[fwk.EntityKey]*upstreamsync.PodGroupAlgorithmResult,
) []SchedulingResult {
	if utilfeature.DefaultFeatureGate.Enabled(features.CompositePodGroup) && root.GetType() == fwk.CompositePodGroupKeyType {
		upstreamsync.CompleteCompositePodGroupAlgorithmResult(root, podGroupCycleState, algResultsMap)
	} else {
		upstreamsync.CompletePodGroupAlgorithmResult(podGroupCycleState, algResultsMap[upstreamsync.GetEntityKey(root)])
	}
	var results []SchedulingResult
	var walk func(pgi *framework.PodGroupInfo)
	walk = func(pgi *framework.PodGroupInfo) {
		if pgi == nil {
			return
		}
		for _, child := range pgi.Children {
			walk(child)
		}
		groupResult, ok := algResultsMap[upstreamsync.GetEntityKey(pgi)]
		if !ok || groupResult == nil {
			return
		}
		for _, pRes := range groupResult.PodResults {
			results = append(results, SchedulingResult{
				Pod:              pRes.GetPod(),
				Status:           pRes.GetStatus(),
				SelectedNodeName: "",
				CycleState:       pRes.GetCycleState(),
			})
		}
	}
	walk(root)
	return results
}

func buildWorkloadPodResults(
	assignments *fwk.PodGroupAssignments,
) []SchedulingResult {
	var results []SchedulingResult
	if assignments != nil {
		results = make([]SchedulingResult, 0, len(assignments.ProposedAssignments))
		for _, pa := range assignments.ProposedAssignments {
			results = append(results, SchedulingResult{
				Pod:              pa.GetPod(),
				Status:           nil,
				SelectedNodeName: pa.GetNodeName(),
				CycleState:       pa.GetCycleState(),
			})
		}
	}
	return results
}

// AddPodGroup adds a pod group object to the snapshot, linking it to its parent
// composite pod group if CompositePodGroup is enabled.
func (s *ClusterSnapshot) AddPodGroup(ctx context.Context, pg *schedulingv1beta1.PodGroup) error {
	if pg == nil {
		return fmt.Errorf("pod group is nil")
	}
	pgCopy := pg.DeepCopy()
	if err := s.addPodGroup(pgCopy); err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	s.undoLog.registerOperation(func() {
		if _, err := s.removePodGroup(pgCopy); err != nil {
			logger.Error(err, "failed to remove pod group during state revert", "podGroup", klog.KObj(pgCopy))
		}
	})
	s.stateVersionForPreemption++
	return nil
}

// RemovePodGroup removes a pod group object from the snapshot, unlinking it from its
// parent composite pod group if CompositePodGroup is enabled.
func (s *ClusterSnapshot) RemovePodGroup(ctx context.Context, pg *schedulingv1beta1.PodGroup) error {
	if pg == nil {
		return fmt.Errorf("pod group is nil")
	}
	removedPG, err := s.removePodGroup(pg)
	if err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	s.undoLog.registerOperation(func() {
		if err := s.addPodGroup(removedPG); err != nil {
			logger.Error(err, "failed to restore pod group during state revert", "podGroup", klog.KObj(removedPG))
		}
	})
	s.stateVersionForPreemption++
	return nil
}

// AddCompositePodGroup adds a composite pod group object to the snapshot, linking it
// to its parent composite pod group if present.
func (s *ClusterSnapshot) AddCompositePodGroup(ctx context.Context, cpg *schedulingv1alpha3.CompositePodGroup) error {
	if cpg == nil {
		return fmt.Errorf("composite pod group is nil")
	}
	cpgCopy := cpg.DeepCopy()
	if err := s.addCompositePodGroup(cpgCopy); err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	s.undoLog.registerOperation(func() {
		if _, err := s.removeCompositePodGroup(cpgCopy); err != nil {
			logger.Error(err, "failed to remove composite pod group during state revert", "compositePodGroup", klog.KObj(cpgCopy))
		}
	})
	s.stateVersionForPreemption++
	return nil
}

// RemoveCompositePodGroup removes a composite pod group object from the snapshot,
// unlinking it from its parent composite pod group if present.
func (s *ClusterSnapshot) RemoveCompositePodGroup(ctx context.Context, cpg *schedulingv1alpha3.CompositePodGroup) error {
	if cpg == nil {
		return fmt.Errorf("composite pod group is nil")
	}
	removedCPG, err := s.removeCompositePodGroup(cpg)
	if err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	s.undoLog.registerOperation(func() {
		if err := s.addCompositePodGroup(removedCPG); err != nil {
			logger.Error(err, "failed to restore composite pod group during state revert", "compositePodGroup", klog.KObj(removedCPG))
		}
	})
	s.stateVersionForPreemption++
	return nil
}

// UPSTREAM-DIFF:
//   - Upstream kubernetes PR #142177 (https://github.com/kubernetes/kubernetes/pull/142177)
//     adds AddGenericPodGroup and RemoveGenericPodGroup directly to cache.Snapshot.
//   - Upstream kubernetes PR #144202 (https://github.com/kubernetes/kubernetes/pull/144202)
//     adds AddPodGroupMember, UpdatePodGroupMember and RemovePodGroupMember directly to cache.Snapshot.
//
// Migration Plan:
// When go.mod updates k8s.io/kubernetes to a version that includes PR #142177 and PR #144202:
// 1. Delete all reflection helper functions and types below:
//    - writableField
//    - addPodGroup
//    - removePodGroup
//    - addCompositePodGroup
//    - removeCompositePodGroup
//    - addPodGroupMember
//    - removePodGroupMember
//    - podGroupKeyOf
//    - podGroupStateWithMember
//    - deletePodGroupState
//    - podGroupStateData and its methods
//    - podGroupStateDataOf
//    - addChildToParent
//    - removeChildFromParent
//    - getOrCreatePodGroupState
//    - getOrCreateCompositePodGroupState
//    - podGroupEmpty
//    - compositePodGroupEmpty
// 2. Change AddPodGroup and AddCompositePodGroup to call s.schedulerSnapshot.AddGenericPodGroup.
//    Preserve DeepCopy() on input objects because AddGenericPodGroup does not copy the passed object.
// 3. Change RemovePodGroup and RemoveCompositePodGroup to look up the existing object in the
//    snapshot first, pass that existing object to s.schedulerSnapshot.RemoveGenericPodGroup,
//    and store existing.DeepCopy() in the undo log.
// 4. Change addUnscheduledPodGroupMembers to call s.schedulerSnapshot.AddPodGroupMember and
//    s.schedulerSnapshot.RemovePodGroupMember instead of addPodGroupMember and removePodGroupMember.
// 5. Keep the public ClusterSnapshot methods unchanged so callers do not break.

func writableField(v reflect.Value, name string) reflect.Value {
	f := v.FieldByName(name)
	return reflect.NewAt(f.Type(), f.Addr().UnsafePointer()).Elem()
}

// addPodGroupMember adds the pod to its pod group state in the snapshot,
// creating the state if it does not exist yet.
// Depending on the NodeName, the pod is tracked as either assigned or unscheduled.
// It returns an error if the pod is already in its pod group state.
// The pod is stored by reference and must not be mutated afterwards.
// The change only affects the snapshot and is reverted by the next Cache.UpdateSnapshot.
// This function is not thread safe, so it should be executed when no other routines can write/read from the snapshot.
//
// UPSTREAM-DIFF: copy of Snapshot.AddPodGroupMember from PR #144202, accessing the unexported
// fields of cache.Snapshot through reflection. It doesn't bump the generation, so callers must revert
// the change through the undo log rather than rely on Cache.UpdateSnapshot.
func (s *ClusterSnapshot) addPodGroupMember(pod *v1.Pod) error {
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()
	if !rv.FieldByName("genericWorkloadEnabled").Bool() {
		return fmt.Errorf("generic workload feature gate is disabled")
	}
	key, err := podGroupKeyOf(pod)
	if err != nil {
		return err
	}
	state := s.getOrCreatePodGroupState(rv, key)
	pgs := podGroupStateDataOf(state)
	if _, ok := pgs.allPods[pod.UID]; ok {
		return fmt.Errorf("pod %s already exists in pod group state %s", klog.KObj(pod), key)
	}
	pgs.addPod(pod)
	return nil
}

// removePodGroupMember removes the pod from its pod group state in the snapshot,
// deleting the state once it has neither pods nor a PodGroup object.
// The change only affects the snapshot and is reverted by the next Cache.UpdateSnapshot.
// This function is not thread safe, so it should be executed when no other routines can write/read from the snapshot.
//
// UPSTREAM-DIFF: copy of Snapshot.RemovePodGroupMember from PR #144202, accessing the unexported
// fields of cache.Snapshot through reflection. It doesn't bump the generation, so callers must revert
// the change through the undo log rather than rely on Cache.UpdateSnapshot.
func (s *ClusterSnapshot) removePodGroupMember(pod *v1.Pod) error {
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()
	if !rv.FieldByName("genericWorkloadEnabled").Bool() {
		return fmt.Errorf("generic workload feature gate is disabled")
	}
	key, err := podGroupKeyOf(pod)
	if err != nil {
		return err
	}
	pgs, err := podGroupStateWithMember(rv, key, pod)
	if err != nil {
		return err
	}
	pgs.deletePod(pod.UID)
	if pgs.empty() {
		deletePodGroupState(rv, key)
	}
	return nil
}

func podGroupKeyOf(pod *v1.Pod) (fwk.EntityKey, error) {
	if len(pod.UID) == 0 {
		return fwk.EntityKey{}, fmt.Errorf("pod %s has an empty UID", klog.KObj(pod))
	}
	if pod.Spec.SchedulingGroup == nil || pod.Spec.SchedulingGroup.PodGroupName == nil {
		return fwk.EntityKey{}, fmt.Errorf("pod %s does not belong to a pod group", klog.KObj(pod))
	}
	return fwk.PodGroupKey(pod.Namespace, *pod.Spec.SchedulingGroup.PodGroupName), nil
}

func podGroupStateWithMember(rv reflect.Value, key fwk.EntityKey, pod *v1.Pod) (podGroupStateData, error) {
	state := writableField(rv, "podGroupStates").MapIndex(reflect.ValueOf(key))
	if !state.IsValid() {
		return podGroupStateData{}, fmt.Errorf("pod group state %s not found in snapshot", key)
	}
	pgs := podGroupStateDataOf(state)
	if _, ok := pgs.allPods[pod.UID]; !ok {
		return podGroupStateData{}, fmt.Errorf("pod %s not found in pod group state %s", klog.KObj(pod), key)
	}
	return pgs, nil
}

// deletePodGroupState removes the pod group state with the given key from rv, the reflected cache.Snapshot.
func deletePodGroupState(rv reflect.Value, key fwk.EntityKey) {
	writableField(rv, "podGroupStates").SetMapIndex(reflect.ValueOf(key), reflect.Value{})
}

// podGroupStateData gives access to the pod tracking fields of a pod group state stored in cache.Snapshot.
// The maps and sets are shared with the pod group state, so changes made through them apply to the snapshot.
// addPod, deletePod, and empty are methods similar to internal methods of cache.podGroupStateData in k8s.io/kubernetes v1.37.0.
//
// UPSTREAM-DIFF: unlike upstream, addPod and deletePod leave the generation unchanged.
// Cache.UpdateSnapshot skips the pod group states whose generation matches the cache's, so it may keep these changes;
// they are reverted through the undo log instead, which ClusterState.SyncSnapshot runs (ResetMutations) before Cache.UpdateSnapshot.
type podGroupStateData struct {
	// state is the *podGroupStateSnapshot value in cache.Snapshot's podGroupStates map.
	state           reflect.Value
	allPods         map[types.UID]*v1.Pod
	unscheduledPods sets.Set[types.UID]
	assumedPods     map[types.UID]*v1.Pod
	assignedPods    sets.Set[types.UID]
}

// podGroupStateDataOf reads the pod tracking fields of the given *podGroupStateSnapshot value.
func podGroupStateDataOf(state reflect.Value) podGroupStateData {
	data := state.Elem()
	return podGroupStateData{
		state:           state,
		allPods:         writableField(data, "allPods").Interface().(map[types.UID]*v1.Pod),
		unscheduledPods: writableField(data, "unscheduledPods").Interface().(sets.Set[types.UID]),
		assumedPods:     writableField(data, "assumedPods").Interface().(map[types.UID]*v1.Pod),
		assignedPods:    writableField(data, "assignedPods").Interface().(sets.Set[types.UID]),
	}
}

func (d podGroupStateData) addPod(pod *v1.Pod) {
	d.allPods[pod.UID] = pod
	if pod.Spec.NodeName != "" {
		d.assignedPods.Insert(pod.UID)
		d.unscheduledPods.Delete(pod.UID)
		delete(d.assumedPods, pod.UID)
	} else {
		d.unscheduledPods.Insert(pod.UID)
	}
}

func (d podGroupStateData) deletePod(podUID types.UID) {
	delete(d.allPods, podUID)
	d.unscheduledPods.Delete(podUID)
	delete(d.assumedPods, podUID)
	d.assignedPods.Delete(podUID)
}

func (d podGroupStateData) empty() bool {
	return podGroupEmpty(d.state)
}

func (s *ClusterSnapshot) addPodGroup(pg *schedulingv1beta1.PodGroup) error {
	key := fwk.PodGroupKey(pg.Namespace, pg.Name)
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()

	pgs := s.getOrCreatePodGroupState(rv, key)
	pgField := writableField(pgs.Elem(), "podGroup")
	if !pgField.IsNil() {
		return fmt.Errorf("pod group %s already exists in snapshot", key)
	}
	pgField.Set(reflect.ValueOf(pg))

	s.addChildToParent(rv, key, pg.Namespace, pg.Spec.ParentCompositePodGroupName)
	return nil
}

func (s *ClusterSnapshot) removePodGroup(pg *schedulingv1beta1.PodGroup) (*schedulingv1beta1.PodGroup, error) {
	key := fwk.PodGroupKey(pg.Namespace, pg.Name)
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()
	pgsMap := writableField(rv, "podGroupStates")
	keyVal := reflect.ValueOf(key)

	pgs := pgsMap.MapIndex(keyVal)
	if !pgs.IsValid() || pgs.Elem().FieldByName("podGroup").IsNil() {
		return nil, fmt.Errorf("pod group %s not found in snapshot", key)
	}
	existingPG := writableField(pgs.Elem(), "podGroup").Interface().(*schedulingv1beta1.PodGroup)
	writableField(pgs.Elem(), "podGroup").SetZero()
	if podGroupEmpty(pgs) {
		pgsMap.SetMapIndex(keyVal, reflect.Value{})
	}

	s.removeChildFromParent(rv, key, existingPG.Namespace, existingPG.Spec.ParentCompositePodGroupName)
	return existingPG.DeepCopy(), nil
}

func (s *ClusterSnapshot) addCompositePodGroup(cpg *schedulingv1alpha3.CompositePodGroup) error {
	key := fwk.CompositePodGroupKey(cpg.Namespace, cpg.Name)
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()

	cpgs := s.getOrCreateCompositePodGroupState(rv, key)
	cpgField := writableField(cpgs.Elem(), "compositePodGroup")
	if !cpgField.IsNil() {
		return fmt.Errorf("composite pod group %s already exists in snapshot", key)
	}
	cpgField.Set(reflect.ValueOf(cpg))

	s.addChildToParent(rv, key, cpg.Namespace, cpg.Spec.ParentCompositePodGroupName)
	return nil
}

func (s *ClusterSnapshot) removeCompositePodGroup(cpg *schedulingv1alpha3.CompositePodGroup) (*schedulingv1alpha3.CompositePodGroup, error) {
	key := fwk.CompositePodGroupKey(cpg.Namespace, cpg.Name)
	rv := reflect.ValueOf(s.schedulerSnapshot).Elem()
	cpgsMap := writableField(rv, "compositePodGroupStates")
	keyVal := reflect.ValueOf(key)

	cpgs := cpgsMap.MapIndex(keyVal)
	if !cpgs.IsValid() || cpgs.Elem().FieldByName("compositePodGroup").IsNil() {
		return nil, fmt.Errorf("composite pod group %s not found in snapshot", key)
	}
	existingCPG := writableField(cpgs.Elem(), "compositePodGroup").Interface().(*schedulingv1alpha3.CompositePodGroup)
	writableField(cpgs.Elem(), "compositePodGroup").SetZero()
	if compositePodGroupEmpty(cpgs) {
		cpgsMap.SetMapIndex(keyVal, reflect.Value{})
	}

	s.removeChildFromParent(rv, key, existingCPG.Namespace, existingCPG.Spec.ParentCompositePodGroupName)
	return existingCPG.DeepCopy(), nil
}

func (s *ClusterSnapshot) addChildToParent(rv reflect.Value, childKey fwk.EntityKey, namespace string, parentName *string) {
	if !rv.FieldByName("compositePodGroupEnabled").Bool() || parentName == nil || *parentName == "" {
		return
	}
	parentKey := fwk.CompositePodGroupKey(namespace, *parentName)
	parent := s.getOrCreateCompositePodGroupState(rv, parentKey)
	children := writableField(parent.Elem(), "children").Interface().(sets.Set[fwk.EntityKey])
	children.Insert(childKey)
}

func (s *ClusterSnapshot) removeChildFromParent(rv reflect.Value, childKey fwk.EntityKey, namespace string, parentName *string) {
	if !rv.FieldByName("compositePodGroupEnabled").Bool() || parentName == nil || *parentName == "" {
		return
	}
	parentKey := fwk.CompositePodGroupKey(namespace, *parentName)
	cpgsMap := writableField(rv, "compositePodGroupStates")
	parentKeyVal := reflect.ValueOf(parentKey)
	if parent := cpgsMap.MapIndex(parentKeyVal); parent.IsValid() {
		children := writableField(parent.Elem(), "children").Interface().(sets.Set[fwk.EntityKey])
		children.Delete(childKey)
		if compositePodGroupEmpty(parent) {
			cpgsMap.SetMapIndex(parentKeyVal, reflect.Value{})
		}
	}
}

func (s *ClusterSnapshot) getOrCreatePodGroupState(rv reflect.Value, key fwk.EntityKey) reflect.Value {
	pgsMap := writableField(rv, "podGroupStates")
	if pgsMap.IsNil() {
		pgsMap.Set(reflect.MakeMap(pgsMap.Type()))
	}
	keyVal := reflect.ValueOf(key)
	pgs := pgsMap.MapIndex(keyVal)
	if !pgs.IsValid() {
		pgs = reflect.New(pgsMap.Type().Elem().Elem())
		for _, field := range []string{"allPods", "unscheduledPods", "assumedPods", "assignedPods"} {
			f := writableField(pgs.Elem(), field)
			f.Set(reflect.MakeMap(f.Type()))
		}
		pgsMap.SetMapIndex(keyVal, pgs)
	}
	return pgs
}

func (s *ClusterSnapshot) getOrCreateCompositePodGroupState(rv reflect.Value, key fwk.EntityKey) reflect.Value {
	cpgsMap := writableField(rv, "compositePodGroupStates")
	if cpgsMap.IsNil() {
		cpgsMap.Set(reflect.MakeMap(cpgsMap.Type()))
	}
	keyVal := reflect.ValueOf(key)
	cpgs := cpgsMap.MapIndex(keyVal)
	if !cpgs.IsValid() {
		cpgs = reflect.New(cpgsMap.Type().Elem().Elem())
		writableField(cpgs.Elem(), "children").Set(reflect.ValueOf(sets.New[fwk.EntityKey]()))
		cpgsMap.SetMapIndex(keyVal, cpgs)
	}
	return cpgs
}

func podGroupEmpty(pgs reflect.Value) bool {
	return pgs.Elem().FieldByName("podGroup").IsNil() &&
		pgs.Elem().FieldByName("allPods").Len() == 0
}

func compositePodGroupEmpty(cpgs reflect.Value) bool {
	return cpgs.Elem().FieldByName("compositePodGroup").IsNil() &&
		cpgs.Elem().FieldByName("children").Len() == 0
}
