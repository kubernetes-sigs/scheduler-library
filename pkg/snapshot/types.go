package snapshot

import (
	"context"

	v1 "k8s.io/api/core/v1"
	fwk "k8s.io/kube-scheduler/framework"
)

type SchedulablePod struct {
	Pod *v1.Pod
	// CandidateNodeNames specifies the nodes which are candidates for the pod to be scheduled on.
	CandidateNodeNames []string
}

type SchedulingOptions struct {
	DryRun        bool
	StopOnFailure bool
}

type SchedulingResult struct {
	Pod              *v1.Pod
	Status           *fwk.Status
	SelectedNodeName string
}

type TransactionResult int

const (
	Commit TransactionResult = iota
	Revert
)

// ClusterSnapshot is used for running the simulations.
// Importantly, all operations supported by the snapshot are in-memory and don't modify the actual cluster state.
type ClusterSnapshot interface {
	// Transaction mutates the snapshot according to the provided transaction function.
	// transactionFn returns TransactionResult which determines whether the snapshot state should be changed or reverted.
	// Any mutations outside of transaction will be immediately committed.
	// Nested transactions are not supported yet.
	// In case of error, the transaction will be reverted and TransactionResult will be ignored.
	Transaction(ctx context.Context, transactionFn func() (TransactionResult, error)) error

	// CanSchedulePod checks feasibility of a single pod on the specified nodes.
	// Doesn't affect the snapshot state.
	// Returns the list of nodes where the pod can be scheduled.
	CanSchedulePod(ctx context.Context, pod SchedulablePod) ([]string, error)

	// SchedulePods attempts to schedule the pods on the specified nodes.
	// Updates the snapshot state with the successfully scheduled pods.
	// The output order is the same as the input.
	SchedulePods(ctx context.Context, pods []SchedulablePod, opts SchedulingOptions) ([]SchedulingResult, error)

	// SchedulePodsByTemplate attempts to schedule as many pods matching the template as possible, only moving to the next node if the pod is unschedulable on the current node.
	// This is functionally equivalent to running SchedulePods with a sufficiently high number of duplicated pods.
	// Can help avoid unnecessary allocations and enable certain performance optimizations.
	SchedulePodsByTemplate(ctx context.Context, template *v1.PodTemplateSpec, candidateNodes []string, maxPods int, opts SchedulingOptions) ([]SchedulingResult, error)

	// PreemptPods marks pods as preempted, so they are not taken into consideration during scheduling.
	// Returns PreemptionSnapshot to make it possible to easily undo the effects of preemption on the snapshot.
	PreemptPods(ctx context.Context, pods []*v1.Pod) (PreemptionSnapshot, error)
	// Undos the preemption done by the PreemptPods.
}

// State of snapshot after preemption operation. It allows to undo result of preemption.
// It can unpreempt only if the snapshot state was't changed after preemption operation.
type PreemptionSnapshot interface {
	// Unpreempt undos the preemption done by the PreemptPods.
	// PreemptionSnapshot is only valid if snapshot hasn't been mutated.
	// Reverted transactions or dry-runs don't invalidate the handle.
	// Preempting or unpreempting other pods doesn't invalidate the handle either.
	// Returns error if the snapshot was updated after preemption operation.
	Unpreempt(ctx context.Context) error
}
