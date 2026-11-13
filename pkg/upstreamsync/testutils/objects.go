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

// Package testutils builds the API objects used across the library tests. Fields shared by every
// test object (the namespace, and the pod UID the scheduler cache keys pods by) are filled in
// here, so that test cases only spell out what actually differs between them. Fields that do not
// apply to a test object are passed empty.
//
// Each scheduling policy gets its own constructor, so that a test reads as the kind of object it
// builds. There is deliberately no constructor for a group without a policy: the scheduling
// policy is a union that requires exactly one member, so such an object could not exist in a
// cluster.
//
// The package intentionally does not depend on any other package of the library, so that the
// in-package tests of those packages can import it without creating an import cycle.
package testutils

import (
	v1 "k8s.io/api/core/v1"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

// MakePod returns a pod in the default namespace, with its UID set to the pod name.
// The scheduler cache keys pods by UID, so every pod that is assumed, forgotten or reverted needs
// a non-empty and unique one.
func MakePod(name, podGroupName, cpu string) *v1.Pod {
	pod := st.MakePod().Name(name).Namespace(metav1.NamespaceDefault).UID(name)
	if podGroupName != "" {
		pod = pod.PodGroupName(podGroupName)
	}
	if cpu != "" {
		pod = pod.Req(map[v1.ResourceName]string{v1.ResourceCPU: cpu})
	}
	return pod.Obj()
}

// MakeGangPodGroup returns a pod group with the gang scheduling policy.
func MakeGangPodGroup(name, parentCompositePodGroupName string, minCount int32) *schedulingv1beta1.PodGroup {
	return makePodGroup(name, parentCompositePodGroupName).MinCount(minCount).Obj()
}

// MakeBasicPodGroup returns a pod group with the basic scheduling policy.
func MakeBasicPodGroup(name, parentCompositePodGroupName string) *schedulingv1beta1.PodGroup {
	return makePodGroup(name, parentCompositePodGroupName).BasicPolicy().Obj()
}

func makeCompositePodGroup(name, parentCompositePodGroupName string) *st.CompositePodGroupWrapper {
	cpg := st.MakeCompositePodGroup().Name(name).Namespace(metav1.NamespaceDefault)
	if parentCompositePodGroupName != "" {
		cpg = cpg.ParentCompositePodGroup(parentCompositePodGroupName)
	}
	return cpg
}

// MakeGangCompositePodGroup returns a composite pod group with the gang scheduling policy.
func MakeGangCompositePodGroup(name, parentCompositePodGroupName string, minGroupCount int32) *schedulingv1beta1.CompositePodGroup {
	return makeCompositePodGroup(name, parentCompositePodGroupName).MinGroupCount(minGroupCount).Obj()
}

// MakeBasicCompositePodGroup returns a composite pod group with the basic scheduling policy.
func MakeBasicCompositePodGroup(name, parentCompositePodGroupName string) *schedulingv1beta1.CompositePodGroup {
	return makeCompositePodGroup(name, parentCompositePodGroupName).BasicPolicy().Obj()
}

func makePodGroup(name, parentCompositePodGroupName string) *st.PodGroupWrapper {
	pg := st.MakePodGroup().Name(name).Namespace(metav1.NamespaceDefault)
	if parentCompositePodGroupName != "" {
		pg = pg.ParentCompositePodGroup(parentCompositePodGroupName)
	}
	return pg
}
