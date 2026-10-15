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
	"fmt"
	"reflect"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

/*

This is a proposal for exposing Add/RemovePod function on the snapshot.

This is needed because snapshot may track some of the state internally
(not presented here since we can't access unexported fields without copying a lot of code),
which may need to be updated together with the deletion or addition of the pod.

Snapshot already exposes AssumePod/ForgetPod, but it's specifically for pods that were not originally in the snapshot, which is useful in scheduling (i.e. "Assume" and then undo with "Forget").
We will want to expose AddPod/RemovePod for pods that were originally in the snapshot, which is useful in preemption (i.e. "Remove" and then undo with "Add").

Both cases can be unified under the same Add/Remove operations.

UPSTREAM-DIFF: the whole file is library-only. It has no counterpart in kube-scheduler yet; it
lives here because it is proposed for the upstream snapshot, not because it was copied from it.

*/

// MutatingSnapshot wraps a cache.Snapshot with Add/RemovePod operations that keep track of what
// was changed, so that the snapshot can be restored to the state it had when the wrapper was
// created (see RestoreState).
type MutatingSnapshot struct {
	*cache.Snapshot
	addedPods   map[string]func()
	removedPods map[string]func()
}

// NewMutatingSnapshot wraps a cache.Snapshot with reversible AddPod and RemovePod operations.
func NewMutatingSnapshot(snapshot *cache.Snapshot) *MutatingSnapshot {
	return &MutatingSnapshot{
		Snapshot:    snapshot,
		addedPods:   map[string]func(){},
		removedPods: map[string]func(){},
	}
}

// RemovePod removes a pod from its assigned node in the snapshot without advancing node generation.
func (s *MutatingSnapshot) RemovePod(logger klog.Logger, podInfo *framework.PodInfo) error {
	key, err := framework.GetPodKey(podInfo.Pod)
	if err != nil {
		return err
	}

	ni, err := s.GetNodeByPod(podInfo.Pod)
	if err != nil {
		return err
	}

	fni, ok := ni.(*framework.NodeInfo)
	if !ok {
		return fmt.Errorf("unknown node info type: %T", ni)
	}
	oldGen := fni.Generation
	defer func() {
		fni.Generation = oldGen
	}()
	err = ni.RemovePod(logger, podInfo.Pod)
	if err != nil {
		return err
	}

	if _, ok := s.addedPods[key]; ok {
		delete(s.addedPods, key)
	} else {
		s.removedPods[key] = func() { _ = s.AddPod(logger, podInfo) }
	}
	return nil
}

// AddPod adds a pod to its assigned node in the snapshot without advancing node generation.
func (s *MutatingSnapshot) AddPod(logger klog.Logger, podInfo *framework.PodInfo) error {
	key, err := framework.GetPodKey(podInfo.Pod)
	if err != nil {
		return err
	}

	ni, err := s.GetNodeByPod(podInfo.Pod)
	if err != nil {
		return err
	}

	fni, ok := ni.(*framework.NodeInfo)
	if !ok {
		return fmt.Errorf("unknown node info type: %T", ni)
	}
	oldGen := fni.Generation
	defer func() {
		fni.Generation = oldGen
	}()
	ni.AddPodInfo(podInfo)

	if _, ok := s.removedPods[key]; ok {
		delete(s.removedPods, key)
	} else {
		s.addedPods[key] = func() { _ = s.RemovePod(logger, podInfo) }
	}
	return nil
}

// RestoreState reverts all pod additions and removals recorded on the MutatingSnapshot.
func (s *MutatingSnapshot) RestoreState() {
	for _, restoreFn := range s.addedPods {
		restoreFn()
	}
	for _, restoreFn := range s.removedPods {
		restoreFn()
	}

	s.addedPods = map[string]func(){}
	s.removedPods = map[string]func(){}
}

// GetNodeByPod returns the NodeInfo for the node specified in pod.Spec.NodeName.
func (s *MutatingSnapshot) GetNodeByPod(pod *v1.Pod) (fwk.NodeInfo, error) {
	nodeName := pod.Spec.NodeName
	ni, err := s.Get(nodeName)
	if err != nil {
		return nil, err
	}
	return ni, nil
}

// UPSTREAM-DIFF: Upstream kubernetes PR #142177 (https://github.com/kubernetes/kubernetes/pull/142177)
// adds AddGenericPodGroup and RemoveGenericPodGroup directly to cache.Snapshot.
//
// Migration Plan:
// When go.mod updates k8s.io/kubernetes to a version that includes PR #142177:
// 1. Delete Snapshot and all reflection helper functions below:
//    - Snapshot
//    - NewSnapshot
//    - WritableField
//    - (s *Snapshot) AddPodGroup
//    - (s *Snapshot) RemovePodGroup
//    - (s *Snapshot) AddCompositePodGroup
//    - (s *Snapshot) RemoveCompositePodGroup
//    - addChildToParent
//    - removeChildFromParent
//    - getOrCreatePodGroupState
//    - getOrCreateCompositePodGroupState
//    - PodGroupEmpty
//    - CompositePodGroupEmpty
// 2. Change ClusterSnapshot.schedulerSnapshot back to *cache.Snapshot, and update
//    ClusterSnapshot.AddPodGroup and AddCompositePodGroup to call s.schedulerSnapshot.AddGenericPodGroup.
//    Preserve DeepCopy() on input objects because AddGenericPodGroup does not copy the passed object.
// 3. Change ClusterSnapshot.RemovePodGroup and RemoveCompositePodGroup to look up the existing object in the
//    snapshot first, pass that existing object to s.schedulerSnapshot.RemoveGenericPodGroup,
//    and store existing.DeepCopy() in the undo log.
// 4. Keep the public ClusterSnapshot methods unchanged so callers do not break.

// Snapshot wraps cache.Snapshot with PodGroup and CompositePodGroup mutation methods
// until PR #142177 is available in k8s.io/kubernetes.
type Snapshot struct {
	*cache.Snapshot
}

// NewSnapshot wraps a cache.Snapshot in a Snapshot.
func NewSnapshot(s *cache.Snapshot) *Snapshot {
	return &Snapshot{Snapshot: s}
}

// WritableField returns a writable reflect.Value for an unexported struct field.
func WritableField(v reflect.Value, name string) reflect.Value {
	f := v.FieldByName(name)
	return reflect.NewAt(f.Type(), f.Addr().UnsafePointer()).Elem()
}

// AddPodGroup adds a PodGroup to cache.Snapshot using reflection until PR #142177 is available in k8s.io/kubernetes.
func (s *Snapshot) AddPodGroup(pg *schedulingv1beta1.PodGroup) error {
	key := fwk.PodGroupKey(pg.Namespace, pg.Name)
	rv := reflect.ValueOf(s.Snapshot).Elem()

	pgs := getOrCreatePodGroupState(rv, key)
	pgField := WritableField(pgs.Elem(), "podGroup")
	if !pgField.IsNil() {
		return fmt.Errorf("pod group %s already exists in snapshot", key)
	}
	pgField.Set(reflect.ValueOf(pg))

	addChildToParent(rv, key, pg.Namespace, pg.Spec.ParentCompositePodGroupName)
	return nil
}

// RemovePodGroup removes a PodGroup from cache.Snapshot using reflection until PR #142177 is available in k8s.io/kubernetes.
func (s *Snapshot) RemovePodGroup(pg *schedulingv1beta1.PodGroup) (*schedulingv1beta1.PodGroup, error) {
	key := fwk.PodGroupKey(pg.Namespace, pg.Name)
	rv := reflect.ValueOf(s.Snapshot).Elem()
	pgsMap := WritableField(rv, "podGroupStates")
	keyVal := reflect.ValueOf(key)

	pgs := pgsMap.MapIndex(keyVal)
	if !pgs.IsValid() || pgs.Elem().FieldByName("podGroup").IsNil() {
		return nil, fmt.Errorf("pod group %s not found in snapshot", key)
	}
	existingPG := WritableField(pgs.Elem(), "podGroup").Interface().(*schedulingv1beta1.PodGroup)
	WritableField(pgs.Elem(), "podGroup").SetZero()
	if PodGroupEmpty(pgs) {
		pgsMap.SetMapIndex(keyVal, reflect.Value{})
	}

	removeChildFromParent(rv, key, existingPG.Namespace, existingPG.Spec.ParentCompositePodGroupName)
	return existingPG.DeepCopy(), nil
}

// AddCompositePodGroup adds a CompositePodGroup to cache.Snapshot using reflection until PR #142177 is available in k8s.io/kubernetes.
func (s *Snapshot) AddCompositePodGroup(cpg *schedulingv1alpha3.CompositePodGroup) error {
	key := fwk.CompositePodGroupKey(cpg.Namespace, cpg.Name)
	rv := reflect.ValueOf(s.Snapshot).Elem()

	cpgs := getOrCreateCompositePodGroupState(rv, key)
	cpgField := WritableField(cpgs.Elem(), "compositePodGroup")
	if !cpgField.IsNil() {
		return fmt.Errorf("composite pod group %s already exists in snapshot", key)
	}
	cpgField.Set(reflect.ValueOf(cpg))

	addChildToParent(rv, key, cpg.Namespace, cpg.Spec.ParentCompositePodGroupName)
	return nil
}

// RemoveCompositePodGroup removes a CompositePodGroup from cache.Snapshot using reflection until PR #142177 is available in k8s.io/kubernetes.
func (s *Snapshot) RemoveCompositePodGroup(cpg *schedulingv1alpha3.CompositePodGroup) (*schedulingv1alpha3.CompositePodGroup, error) {
	key := fwk.CompositePodGroupKey(cpg.Namespace, cpg.Name)
	rv := reflect.ValueOf(s.Snapshot).Elem()
	cpgsMap := WritableField(rv, "compositePodGroupStates")
	keyVal := reflect.ValueOf(key)

	cpgs := cpgsMap.MapIndex(keyVal)
	if !cpgs.IsValid() || cpgs.Elem().FieldByName("compositePodGroup").IsNil() {
		return nil, fmt.Errorf("composite pod group %s not found in snapshot", key)
	}
	existingCPG := WritableField(cpgs.Elem(), "compositePodGroup").Interface().(*schedulingv1alpha3.CompositePodGroup)
	WritableField(cpgs.Elem(), "compositePodGroup").SetZero()
	if CompositePodGroupEmpty(cpgs) {
		cpgsMap.SetMapIndex(keyVal, reflect.Value{})
	}

	removeChildFromParent(rv, key, existingCPG.Namespace, existingCPG.Spec.ParentCompositePodGroupName)
	return existingCPG.DeepCopy(), nil
}

func addChildToParent(rv reflect.Value, childKey fwk.EntityKey, namespace string, parentName *string) {
	if !rv.FieldByName("compositePodGroupEnabled").Bool() || parentName == nil || *parentName == "" {
		return
	}
	parentKey := fwk.CompositePodGroupKey(namespace, *parentName)
	parent := getOrCreateCompositePodGroupState(rv, parentKey)
	children := WritableField(parent.Elem(), "children").Interface().(sets.Set[fwk.EntityKey])
	children.Insert(childKey)
}

func removeChildFromParent(rv reflect.Value, childKey fwk.EntityKey, namespace string, parentName *string) {
	if !rv.FieldByName("compositePodGroupEnabled").Bool() || parentName == nil || *parentName == "" {
		return
	}
	parentKey := fwk.CompositePodGroupKey(namespace, *parentName)
	cpgsMap := WritableField(rv, "compositePodGroupStates")
	parentKeyVal := reflect.ValueOf(parentKey)
	if parent := cpgsMap.MapIndex(parentKeyVal); parent.IsValid() {
		children := WritableField(parent.Elem(), "children").Interface().(sets.Set[fwk.EntityKey])
		children.Delete(childKey)
		if CompositePodGroupEmpty(parent) {
			cpgsMap.SetMapIndex(parentKeyVal, reflect.Value{})
		}
	}
}

func getOrCreatePodGroupState(rv reflect.Value, key fwk.EntityKey) reflect.Value {
	pgsMap := WritableField(rv, "podGroupStates")
	if pgsMap.IsNil() {
		pgsMap.Set(reflect.MakeMap(pgsMap.Type()))
	}
	keyVal := reflect.ValueOf(key)
	pgs := pgsMap.MapIndex(keyVal)
	if !pgs.IsValid() {
		pgs = reflect.New(pgsMap.Type().Elem().Elem())
		for _, field := range []string{"allPods", "unscheduledPods", "assumedPods", "assignedPods"} {
			f := WritableField(pgs.Elem(), field)
			f.Set(reflect.MakeMap(f.Type()))
		}
		pgsMap.SetMapIndex(keyVal, pgs)
	}
	return pgs
}

func getOrCreateCompositePodGroupState(rv reflect.Value, key fwk.EntityKey) reflect.Value {
	cpgsMap := WritableField(rv, "compositePodGroupStates")
	if cpgsMap.IsNil() {
		cpgsMap.Set(reflect.MakeMap(cpgsMap.Type()))
	}
	keyVal := reflect.ValueOf(key)
	cpgs := cpgsMap.MapIndex(keyVal)
	if !cpgs.IsValid() {
		cpgs = reflect.New(cpgsMap.Type().Elem().Elem())
		WritableField(cpgs.Elem(), "children").Set(reflect.ValueOf(sets.New[fwk.EntityKey]()))
		cpgsMap.SetMapIndex(keyVal, cpgs)
	}
	return cpgs
}

// PodGroupEmpty reports whether a podGroupStateSnapshot has no PodGroup object and no pods.
func PodGroupEmpty(pgs reflect.Value) bool {
	return pgs.Elem().FieldByName("podGroup").IsNil() &&
		pgs.Elem().FieldByName("allPods").Len() == 0
}

// CompositePodGroupEmpty reports whether a compositePodGroupStateSnapshot has no CompositePodGroup object and no children.
func CompositePodGroupEmpty(cpgs reflect.Value) bool {
	return cpgs.Elem().FieldByName("compositePodGroup").IsNil() &&
		cpgs.Elem().FieldByName("children").Len() == 0
}
