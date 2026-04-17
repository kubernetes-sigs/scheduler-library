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

	"github.com/google/uuid"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

// clusterSnapshot implements ClusterSnapshot interface as a stub.
var _ ClusterSnapshot = (*clusterSnapshot)(nil)

type clusterSnapshot struct {
	schedulerSnapshot *cache.Snapshot
	frameworks        map[string]schedulerframework.Framework
	transactions      []string
	lastCommittedTx    string
	txCompensation    map[string][]func() error
}

// NewClusterSnapshot creates a new ClusterSnapshot stub wrapping the provided scheduler snapshot and frameworks.
func NewClusterSnapshot(s *cache.Snapshot, frameworks map[string]schedulerframework.Framework) ClusterSnapshot {
	return &clusterSnapshot{
		schedulerSnapshot: s,
		frameworks:        frameworks,
		txCompensation:    make(map[string][]func() error),
	}
}

// Transaction executes the provided function within a transaction.
// It rolls back operations if the function returns Revert or an error.
func (s *clusterSnapshot) Transaction(ctx context.Context, transactionFn func() (TransactionResult, error)) error {
	txId := uuid.New().String()
	s.transactions = append(s.transactions, txId)
	s.txCompensation[txId] = []func() error{}

	defer func() {
		delete(s.txCompensation, txId)
		s.transactions = s.transactions[:len(s.transactions)-1]
	}()

	result, err := transactionFn()

	if err != nil || result == Revert {
		operations := s.txCompensation[txId]
		for i := len(operations) - 1; i >= 0; i-- {
			if rErr := operations[i](); rErr != nil {
				if err != nil {
					return fmt.Errorf("transaction failed (%w) and revert failed at index %d: %v", err, i, rErr)
				}
				return fmt.Errorf("failed to revert operation at index %d: %w", i, rErr)
			}
		}
	} else {
		s.lastCommittedTx = txId
	}

	return err
}

// CanSchedulePod checks feasibility of a single pod on the specified nodes.
func (s *clusterSnapshot) CanSchedulePod(ctx context.Context, pod SchedulablePod) ([]string, error) {
	fwk, err := s.getFramework(pod.Pod.Spec.SchedulerName)
	if err != nil {
		return nil, err
	}
	state := schedulerframework.NewCycleState()

	preFilterResult, status, _ := fwk.RunPreFilterPlugins(ctx, state, pod.Pod)
	if !status.IsSuccess() {
		if status.IsRejected() {
			return nil, nil // Not feasible on any node
		}
		return nil, status.AsError()
	}

	var feasibleNodes []string
	if preFilterResult == nil || preFilterResult.AllNodes() {
		feasibleNodes = pod.CandidateNodeNames
	} else {
		for _, nodeName := range pod.CandidateNodeNames {
			if preFilterResult.NodeNames.Has(nodeName) {
				feasibleNodes = append(feasibleNodes, nodeName)
			}
		}
	}

	return feasibleNodes, nil
}

// SchedulePods is a stub implementation.
func (s *clusterSnapshot) SchedulePods(ctx context.Context, pods []SchedulablePod, opts SchedulingOptions) ([]SchedulingResult, error) {
	logger := klog.FromContext(ctx)

	var currTx []func() error
	if opts.DryRun {
		currTx = []func() error{}
		defer func() {
			for i := len(currTx) - 1; i >= 0; i-- {
				if err := currTx[i](); err != nil {
					logger.Error(err, "Failed to revert operation", "index", i)
				}
			}
		}()
	}

	result := make([]SchedulingResult, 0)
	if len(pods) == 0 {
		return result, nil
	}

	framework, err := s.getFramework(pods[0].Pod.Spec.SchedulerName)
	if err != nil {
		return nil, err
	}

	for _, pod := range pods {
		cycleState := schedulerframework.NewCycleState()

		prefilterResult, status, _ := framework.RunPreFilterPlugins(ctx, cycleState, pod.Pod)
		if !status.IsSuccess() {
			if opts.StopOnFailure {
				return result, status.AsError()
			}
			continue
		}

		success := false
		for _, nodeName := range pod.CandidateNodeNames {
			if prefilterResult != nil && !prefilterResult.AllNodes() && !prefilterResult.NodeNames.Has(nodeName) {
				continue
			}

			node, err := framework.SnapshotSharedLister().NodeInfos().Get(nodeName)
			if err != nil {
				return nil, err
			}
			status := framework.RunFilterPlugins(ctx, cycleState, pod.Pod, node)
			if status.IsSuccess() {
				result = append(result, SchedulingResult{
					Pod:              pod.Pod,
					Status:           status,
					SelectedNodeName: nodeName,
				})
				err := s.assumeAndReserve(ctx, pod.Pod, nodeName, cycleState)
				if err != nil {
					unreserveErr := s.unreserveAndForget(ctx, pod.Pod, nodeName, cycleState)
					if unreserveErr != nil {
						return result, unreserveErr
					}
					return result, err
				}
				if opts.DryRun {
					currTx = append(currTx, func() error {
						return s.unreserveAndForget(ctx, pod.Pod, nodeName, cycleState)
					})
				} else if len(s.transactions) > 0 {
					txId := s.transactions[len(s.transactions)-1]
					s.txCompensation[txId] = append(s.txCompensation[txId], func() error {
						return s.unreserveAndForget(ctx, pod.Pod, nodeName, cycleState)
					})
				}
				success = true
				break
			}
		}
		if !success {
			if opts.StopOnFailure {
				return result, fmt.Errorf("pod %s could not be scheduled on any candidate node", klog.KObj(pod.Pod))
			}
		}
	}

	return result, nil
}

// SchedulePodsByTemplate attempts to schedule as many pods matching the template as possible.
// It assumes candidate nodes are feasible and moves to the next node only if the pod is unschedulable on the current node.
func (s *clusterSnapshot) SchedulePodsByTemplate(ctx context.Context, template *v1.PodTemplateSpec, candidateNodes []string, maxPods int, opts SchedulingOptions) ([]SchedulingResult, error) {
	logger := klog.FromContext(ctx)

	var currTx []func() error
	if opts.DryRun {
		currTx = []func() error{}
		defer func() {
			for i := len(currTx) - 1; i >= 0; i-- {
				if err := currTx[i](); err != nil {
					logger.Error(err, "Failed to revert operation", "index", i)
				}
			}
		}()
	}

	result := make([]SchedulingResult, 0)
	if maxPods <= 0 || len(candidateNodes) == 0 {
		return result, nil
	}

	framework, err := s.getFramework(template.Spec.SchedulerName)
	if err != nil {
		return nil, err
	}

	podNamePrefix := template.Name
	if podNamePrefix == "" {
		podNamePrefix = "templated-pod"
	}
	ns := template.Namespace
	if ns == "" {
		ns = "default"
	}

	nodeIdx := 0
	for i := 0; i < maxPods; i++ {
		pod := &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%d", podNamePrefix, i),
				Namespace: ns,
				UID:       types.UID(uuid.New().String()),
			},
			Spec: template.Spec,
		}

		cycleState := schedulerframework.NewCycleState()
		scheduled := false

		for nodeIdx < len(candidateNodes) {
			nodeName := candidateNodes[nodeIdx]
			node, err := framework.SnapshotSharedLister().NodeInfos().Get(nodeName)
			if err != nil {
				return nil, err
			}

			status := framework.RunFilterPlugins(ctx, cycleState, pod, node)
			if status.IsSuccess() {
				result = append(result, SchedulingResult{
					Pod:              pod,
					Status:           status,
					SelectedNodeName: nodeName,
				})

				err := s.assumeAndReserve(ctx, pod, nodeName, cycleState)
				if err != nil {
					unreserveErr := s.unreserveAndForget(ctx, pod, nodeName, cycleState)
					if unreserveErr != nil {
						return result, unreserveErr
					}
					return result, err
				}

				if opts.DryRun {
					currTx = append(currTx, func() error {
						return s.unreserveAndForget(ctx, pod, nodeName, cycleState)
					})
				} else if len(s.transactions) > 0 {
					txId := s.transactions[len(s.transactions)-1]
					s.txCompensation[txId] = append(s.txCompensation[txId], func() error {
						return s.unreserveAndForget(ctx, pod, nodeName, cycleState)
					})
				}

				scheduled = true
				break // Move to next pod
			} else {
				// Move to next node if pod is unschedulable on current node
				nodeIdx++
			}
		}

		if !scheduled {
			// Could not schedule this pod on any remaining nodes, stop.
			break
		}
	}

	return result, nil
}

// PreemptPods removes pods from the snapshot.
// It supports transaction rollbacks if called inside a transaction.
func (s *clusterSnapshot) PreemptPods(ctx context.Context, pods []*v1.Pod) (PreemptionSnapshot, error) {
	var preemptedPods []*v1.Pod
	var nodeNames []string

	var txId string
	insideTx := len(s.transactions) > 0
	if insideTx {
		txId = s.transactions[len(s.transactions)-1]
	}

	for _, pod := range pods {
		pod := pod // avoid capturing loop variable
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			return newPreemptionSnapshot(s, preemptedPods, nodeNames), fmt.Errorf("pod %s has no node name", klog.KObj(pod))
		}

		cycleState := schedulerframework.NewCycleState()

		err := s.unreserveAndForget(ctx, pod, nodeName, cycleState)
		if err != nil {
			return newPreemptionSnapshot(s, preemptedPods, nodeNames), fmt.Errorf("failed to unreserve and forget pod %s: %w", klog.KObj(pod), err)
		}

		preemptedPods = append(preemptedPods, pod)
		nodeNames = append(nodeNames, nodeName)

		if insideTx {
			s.txCompensation[txId] = append(s.txCompensation[txId], func() error {
				return s.assumeAndReserve(ctx, pod, nodeName, cycleState)
			})
		}
	}

	return newPreemptionSnapshot(s, preemptedPods, nodeNames), nil
}

var _ PreemptionSnapshot = (*preemptionSnapshot)(nil)

type preemptionSnapshot struct {
	snapshot           *clusterSnapshot
	pods               []*v1.Pod
	nodeNames          []string
	currentTx          string
	currentTxMutations int
	lastCommittedTx     string
}

func newPreemptionSnapshot(s *clusterSnapshot, pods []*v1.Pod, nodeNames []string) *preemptionSnapshot {
	var currentTx string
	if len(s.transactions) > 0 {
		currentTx = s.transactions[len(s.transactions)-1]
	}
	var currentTxMutations int
	if currentTx != "" {
		currentTxMutations = len(s.txCompensation[currentTx])
	}

	return &preemptionSnapshot{
		snapshot:           s,
		pods:               pods,
		nodeNames:          nodeNames,
		currentTx:          currentTx,
		currentTxMutations: currentTxMutations,
		lastCommittedTx:     s.lastCommittedTx,
	}
}

// Unpreempt undos the preemption done by the PreemptPods.
func (ps *preemptionSnapshot) Unpreempt(ctx context.Context) error {
	newTxWasCommitedAfterPreemption := ps.lastCommittedTx != ps.snapshot.lastCommittedTx

	var currentSnapshotTx string
	if len(ps.snapshot.transactions) > 0 {
		currentSnapshotTx = ps.snapshot.transactions[len(ps.snapshot.transactions)-1]
	}
	newTxStarted := ps.currentTx != currentSnapshotTx

	newMutationForCurrentTx := !newTxStarted && ps.currentTxMutations != len(ps.snapshot.txCompensation[ps.currentTx])

	if newTxWasCommitedAfterPreemption || newTxStarted || newMutationForCurrentTx {
		return fmt.Errorf("snapshot was mutated after preemption")
	}

	for i, pod := range ps.pods {
		nodeName := ps.nodeNames[i]
		cycleState := schedulerframework.NewCycleState()
		if err := ps.snapshot.assumeAndReserve(ctx, pod, nodeName, cycleState); err != nil {
			return fmt.Errorf("failed to assume and reserve pod %s during unpreempt: %w", klog.KObj(pod), err)
		}
	}

	if ps.currentTx != "" {
		txId := ps.currentTx
		numPods := len(ps.pods)
		if len(ps.snapshot.txCompensation[txId]) >= numPods {
			ps.snapshot.txCompensation[txId] = ps.snapshot.txCompensation[txId][:len(ps.snapshot.txCompensation[txId])-numPods]
		} else {
			return fmt.Errorf("unexpected number of mutations in transaction compensation list")
		}
	}

	return nil
}

func (c *clusterSnapshot) getFramework(schedulerName string) (schedulerframework.Framework, error) {
	if schedulerName == "" {
		schedulerName = v1.DefaultSchedulerName
	}

	framework, ok := c.frameworks[schedulerName]
	if !ok {
		return nil, fmt.Errorf("no framework found for scheduler: %q", schedulerName)
	}

	return framework, nil
}

func (c *clusterSnapshot) assumeAndReserve(ctx context.Context, pod *v1.Pod, nodeName string, cycleState *schedulerframework.CycleState) error {
	framework, err := c.getFramework(pod.Spec.SchedulerName)
	if err != nil {
		return err
	}

	podInfo, err := schedulerframework.NewPodInfo(pod.DeepCopy())
	if err != nil {
		return err
	}
	podInfo.Pod.Spec.NodeName = nodeName

	err = c.schedulerSnapshot.AssumePod(podInfo)
	if err != nil {
		return err
	}
	
	status := framework.RunReservePluginsReserve(ctx, cycleState, pod, nodeName)
	if !status.IsSuccess() {
		return status.AsError()
	}
	return nil
}

func (c *clusterSnapshot) unreserveAndForget(ctx context.Context, pod *v1.Pod, nodeName string, cycleState *schedulerframework.CycleState) error {
	framework, err := c.getFramework(pod.Spec.SchedulerName)
	if err != nil {
		return err
	}

	framework.RunReservePluginsUnreserve(ctx, cycleState, pod, nodeName)
	podInfo, err := schedulerframework.NewPodInfo(pod.DeepCopy())
	if err != nil {
		return err
	}
	podInfo.Pod.Spec.NodeName = nodeName
	err = c.removePod(ctx, podInfo)
	podInfo.Pod.Spec.NodeName = "" // clear after use
	return err
}

func (c *clusterSnapshot) removePod(ctx context.Context, podInfo *schedulerframework.PodInfo) error {
	logger := klog.FromContext(ctx)

	err := c.schedulerSnapshot.ForgetPod(logger, podInfo.Pod)
	if err != nil {
		nodeName := podInfo.Pod.Spec.NodeName
		nodeInfo, getErr := c.schedulerSnapshot.NodeInfos().Get(nodeName)
		if getErr != nil {
			return fmt.Errorf("failed to get node %q: %v", nodeName, getErr)
		}
		if removeErr := nodeInfo.RemovePod(logger, podInfo.Pod); removeErr != nil {
			return fmt.Errorf("failed to remove pod from NodeInfo: %v", removeErr)
		}
	}

	return nil
}
