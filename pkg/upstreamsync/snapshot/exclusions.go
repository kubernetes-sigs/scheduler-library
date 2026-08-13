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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// RejectionStage identifies the evaluation stage at which a node was rejected.
type RejectionStage string

const (
	// RejectionStagePreFilter marks nodes rejected because a PreFilter plugin
	// rejected the pod before any per-node evaluation.
	RejectionStagePreFilter RejectionStage = "PreFilter"
	// RejectionStagePreFilterNarrowing marks nodes removed by PreFilterResult
	// candidate-set narrowing rather than an explicit per-node rejection.
	RejectionStagePreFilterNarrowing RejectionStage = "PreFilterNarrowing"
	// RejectionStageFilter marks nodes rejected by a Filter plugin.
	RejectionStageFilter RejectionStage = "Filter"
	// RejectionStageExtender marks nodes rejected by a scheduling extender.
	RejectionStageExtender RejectionStage = "Extender"
	// RejectionStageUnknown marks rejections for which the diagnosis carries
	// no attribution data.
	RejectionStageUnknown RejectionStage = "Unknown"
)

// NodeExclusion describes why a single placement node was rejected by a
// feasibility check. Plugin is the rejecting plugin's registered name (as
// reported by the scheduling framework) and is empty when the rejection
// cannot be attributed to exactly one plugin; Stage is then the most specific
// stage the diagnosis supports. The record is derived from typed Diagnosis
// data only — status messages are never consulted, so the values are stable
// across message-wording changes in the plugins.
type NodeExclusion struct {
	NodeName string
	Plugin   string
	Stage    RejectionStage
}

// CanSchedulePodWithExclusions is CanSchedulePod plus one structured
// NodeExclusion record per rejected placement node, derived from the returned
// Diagnosis via NodeExclusionsFromDiagnosis. The feasible-node result is
// identical to CanSchedulePod's; on error no exclusions are derived.
func (s *ClusterSnapshot) CanSchedulePodWithExclusions(ctx context.Context, pod *v1.Pod, placement *fwk.Placement) ([]string, []NodeExclusion, *framework.Diagnosis, error) {
	feasibleNodes, diagnosis, err := s.CanSchedulePod(ctx, pod, placement)
	if err != nil {
		return feasibleNodes, nil, diagnosis, err
	}
	return feasibleNodes, NodeExclusionsFromDiagnosis(placement, feasibleNodes, diagnosis), diagnosis, nil
}

// NodeExclusionsFromDiagnosis derives one NodeExclusion per placement node
// that is not in feasibleNodeNames, in placement order. It reads only typed
// Diagnosis data and does not mutate the Diagnosis.
//
// Attribution follows how the framework populates the Diagnosis:
//   - Filter plugins store an explicit per-node status tagged with the
//     rejecting plugin's name.
//   - A PreFilter plugin rejection is stored as the absent-nodes status, also
//     tagged with the plugin name, covering every node without an explicit
//     status.
//   - PreFilterResult narrowing stores a plugin-less absent-nodes status and
//     records the narrowing plugins in UnschedulablePlugins; a plugin is
//     attributed only when exactly one is recorded.
//   - Extenders store explicit plugin-less per-node statuses and add
//     framework.ExtenderName to UnschedulablePlugins.
func NodeExclusionsFromDiagnosis(placement *fwk.Placement, feasibleNodeNames []string, diag *framework.Diagnosis) []NodeExclusion {
	if placement == nil || len(placement.Nodes) == 0 {
		return nil
	}
	feasible := sets.New(feasibleNodeNames...)
	explicit := sets.New[string]()
	if diag != nil && diag.NodeToStatus != nil {
		diag.NodeToStatus.ForEachExplicitNode(func(nodeName string, _ *fwk.Status) {
			explicit.Insert(nodeName)
		})
	}
	var exclusions []NodeExclusion
	for _, nodeInfo := range placement.Nodes {
		node := nodeInfo.Node()
		if node == nil || feasible.Has(node.Name) {
			continue
		}
		exclusions = append(exclusions, nodeExclusion(node.Name, diag, explicit.Has(node.Name)))
	}
	return exclusions
}

func nodeExclusion(nodeName string, diag *framework.Diagnosis, hasExplicitStatus bool) NodeExclusion {
	unknown := NodeExclusion{NodeName: nodeName, Stage: RejectionStageUnknown}
	if diag == nil || diag.NodeToStatus == nil {
		return unknown
	}
	var plugin string
	if status := diag.NodeToStatus.Get(nodeName); status != nil {
		plugin = status.Plugin()
	}
	// ExtenderName is a pseudo-plugin marker, never a narrowing candidate.
	narrowingPlugins := diag.UnschedulablePlugins.Difference(sets.New(framework.ExtenderName))
	switch {
	case hasExplicitStatus && plugin != "":
		return NodeExclusion{NodeName: nodeName, Plugin: plugin, Stage: RejectionStageFilter}
	case hasExplicitStatus:
		if diag.UnschedulablePlugins.Has(framework.ExtenderName) {
			return NodeExclusion{NodeName: nodeName, Stage: RejectionStageExtender}
		}
		return unknown
	case plugin != "":
		return NodeExclusion{NodeName: nodeName, Plugin: plugin, Stage: RejectionStagePreFilter}
	case narrowingPlugins.Len() == 1:
		return NodeExclusion{NodeName: nodeName, Plugin: narrowingPlugins.UnsortedList()[0], Stage: RejectionStagePreFilterNarrowing}
	case narrowingPlugins.Len() > 1:
		return NodeExclusion{NodeName: nodeName, Stage: RejectionStagePreFilterNarrowing}
	default:
		return unknown
	}
}
