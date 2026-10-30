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

package preemption

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	fwk "k8s.io/kube-scheduler/framework"
)

// CommittedVictimPlugin is a Filter plugin that rejects nodes currently occupied
// by already-committed preemption victims during the first pass of ScheduleWorkload.
type CommittedVictimPlugin struct {
}

// TODO: implement SignPlugin to enable opportunistic batching.
var _ fwk.FilterPlugin = &CommittedVictimPlugin{}

// CommittedVictimsPluginName is the framework registration name of CommittedVictimPlugin.
const CommittedVictimsPluginName = "SchedulerLibrary_CommittedVictimsPlugin"
const committedVictimsStateKey = "SchedulerLibrary_CommittedVictimsStateKey"

type committedVictims struct {
	NodesWithVictims sets.Set[string]
}

func (v *committedVictims) Clone() fwk.StateData {
	return &committedVictims{
		NodesWithVictims: v.NodesWithVictims, // readonly, doesn't need to be cloned
	}
}

// Name returns the name of the plugin.
func (c *CommittedVictimPlugin) Name() string {
	return CommittedVictimsPluginName
}

// Filter return unschedulable for nodes occupied by committed victims to check if the pod fits without having to wait for committed victims to be removed.
func (c *CommittedVictimPlugin) Filter(ctx context.Context, state fwk.CycleState, pod *v1.Pod, nodeInfo fwk.NodeInfo) *fwk.Status {
	if v := getCommittedVictims(state); v != nil && v.NodesWithVictims.Has(nodeInfo.Node().Name) {
		return fwk.NewStatus(fwk.Unschedulable, "node is occupied by a committed victim")
	}
	return nil
}

func getRootCycleState(pgState fwk.PodGroupCycleState) fwk.PodGroupCycleState {
	for pgState != nil {
		parentState := pgState.GetParentPlacementCycleState()
		if parentState == nil {
			break
		}
		pgState = parentState.GetPodGroupSchedulingCycle()
	}
	return pgState
}

func getCommittedVictimsFromPodGroupState(pgState fwk.PodGroupCycleState) *committedVictims {
	rootCycleState := getRootCycleState(pgState)
	if rootCycleState == nil {
		return nil
	}
	data, err := rootCycleState.Read(committedVictimsStateKey)
	if err != nil {
		return nil
	}
	return data.(*committedVictims)
}

func getCommittedVictims(state fwk.CycleState) *committedVictims {
	return getCommittedVictimsFromPodGroupState(state.GetPodGroupSchedulingCycle())
}

// SetCommittedVictimsInCycleState validates provided victims and records the set of nodes occupied
// by their pods onto rootCycleState for CommittedVictimPlugin.
func SetCommittedVictimsInCycleState(rootCycleState fwk.CycleState, cvs []PreemptionVictim) error {
	nodesWithVictims := sets.Set[string]{}
	for _, v := range cvs {
		if v == nil || len(v.Pods()) == 0 {
			return fmt.Errorf("committed preemption victim cannot be nil nor empty")
		}
		for _, p := range v.Pods() {
			if p == nil || p.Spec.NodeName == "" {
				return fmt.Errorf("committed preemption victim pod cannot be nil or unscheduled")
			}
			nodesWithVictims.Insert(p.Spec.NodeName)
		}
	}
	rootCycleState.Write(committedVictimsStateKey, &committedVictims{
		NodesWithVictims: nodesWithVictims,
	})
	return nil
}

// HasCommittedVictimsInCycleStateForTest reports whether SetCommittedVictimsInCycleState
// was called on pgState's root cycle state.
func HasCommittedVictimsInCycleStateForTest(pgState fwk.PodGroupCycleState) bool {
	return getCommittedVictimsFromPodGroupState(pgState) != nil
}
