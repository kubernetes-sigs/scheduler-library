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
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
)

// RejectionStage is the step of the scheduling flow that excluded a node.
//
// UPSTREAM-DIFF: library-only, see NodeExclusion.
type RejectionStage string

// Values of RejectionStage.
const (
	RejectionStagePreFilter          RejectionStage = "PreFilter"
	RejectionStagePreFilterNarrowing RejectionStage = "PreFilterNarrowing"
	RejectionStageFilter             RejectionStage = "Filter"
	RejectionStageExtender           RejectionStage = "Extender"
)

// NodeExclusion records the stage and the plugins that excluded a node.
//
// UPSTREAM-DIFF: library-only. Once the flow has returned, Diagnosis.UnschedulablePlugins mixes the
// plugins that narrowed with the ones that rejected nodes in Filter, so the flow records the
// exclusions itself. Tracked in https://github.com/kubernetes-sigs/scheduler-library/issues/15.
type NodeExclusion struct {
	NodeName string
	Stage    RejectionStage
	// Plugins is sorted. It names several plugins when several narrowed together, and none for an
	// extender. It may be shared between records and must not be modified.
	Plugins []string
}

type exclusionRecorder struct {
	exclusions []NodeExclusion
}

func (r *exclusionRecorder) recordPreFilterRejection(nodes []fwk.NodeInfo, status *fwk.Status, narrowingPlugins sets.Set[string]) {
	if r == nil {
		return
	}
	stage, plugins := RejectionStagePreFilter, []string{status.Plugin()}
	// The framework names no plugin only when the PreFilterResults leave no node.
	if status.Plugin() == "" {
		stage, plugins = RejectionStagePreFilterNarrowing, sets.List(narrowingPlugins)
	}
	for _, nodeInfo := range nodes {
		r.record(nodeInfo.Node().Name, stage, plugins)
	}
}

func (r *exclusionRecorder) recordNarrowing(nodes []fwk.NodeInfo, preRes *fwk.PreFilterResult, narrowingPlugins sets.Set[string]) {
	if r == nil {
		return
	}
	plugins := sets.List(narrowingPlugins)
	for _, nodeInfo := range nodes {
		if nodeName := nodeInfo.Node().Name; !preRes.NodeNames.Has(nodeName) {
			r.record(nodeName, RejectionStagePreFilterNarrowing, plugins)
		}
	}
}

func (r *exclusionRecorder) recordFilterRejection(nodeName string, status *fwk.Status) {
	if r == nil {
		return
	}
	r.record(nodeName, RejectionStageFilter, []string{status.Plugin()})
}

func (r *exclusionRecorder) recordExtenderRejections(passedFilters, passedExtenders []fwk.NodeInfo) {
	if r == nil {
		return
	}
	passed := sets.New[string]()
	for _, nodeInfo := range passedExtenders {
		passed.Insert(nodeInfo.Node().Name)
	}
	for _, nodeInfo := range passedFilters {
		if nodeName := nodeInfo.Node().Name; !passed.Has(nodeName) {
			r.record(nodeName, RejectionStageExtender, nil)
		}
	}
}

func (r *exclusionRecorder) record(nodeName string, stage RejectionStage, plugins []string) {
	r.exclusions = append(r.exclusions, NodeExclusion{NodeName: nodeName, Stage: stage, Plugins: plugins})
}
