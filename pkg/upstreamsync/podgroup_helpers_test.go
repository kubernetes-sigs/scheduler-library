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
	"testing"

	v1 "k8s.io/api/core/v1"
	schedulingv1alpha3 "k8s.io/api/scheduling/v1alpha3"
	schedulingv1beta1 "k8s.io/api/scheduling/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/features"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestIsPodGroupMember(t *testing.T) {
	tests := []struct {
		name    string
		pod     *v1.Pod
		wantErr bool
	}{
		{
			name:    "nil pod",
			pod:     nil,
			wantErr: true,
		},
		{
			name:    "pod without scheduling group",
			pod:     st.MakePod().Name("pod-no-sg").Obj(),
			wantErr: true,
		},
		{
			name:    "pod with empty pod group name",
			pod:     st.MakePod().Name("pod-empty-sg").PodGroupName("").Obj(),
			wantErr: true,
		},
		{
			name:    "valid pod with pod group name",
			pod:     st.MakePod().Name("pod-valid").PodGroupName("pg-1").Obj(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := isPodGroupMember(tt.pod)
			if (err != nil) != tt.wantErr {
				t.Errorf("isPodGroupMember() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidatePods(t *testing.T) {
	tests := []struct {
		name          string
		pods          []*v1.Pod
		wantNamespace string
		wantErr       bool
	}{
		{
			name: "valid pods in same namespace",
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns1").PodGroupName("test-pg").Obj(),
				st.MakePod().Name("p2").Namespace("ns1").PodGroupName("test-pg").Obj(),
			},
			wantNamespace: "ns1",
			wantErr:       false,
		},
		{
			name: "pods without namespace defaults to default namespace",
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("").PodGroupName("test-pg").Obj(),
				st.MakePod().Name("p2").Namespace("").PodGroupName("test-pg").Obj(),
			},
			wantNamespace: metav1.NamespaceDefault,
			wantErr:       false,
		},
		{
			name: "pods in different namespaces return error",
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns1").PodGroupName("test-pg").Obj(),
				st.MakePod().Name("p2").Namespace("ns2").PodGroupName("test-pg").Obj(),
			},
			wantErr: true,
		},
		{
			name: "pod without UID generates non-empty UID",
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns1").UID("").PodGroupName("test-pg").Obj(),
			},
			wantNamespace: "ns1",
			wantErr:       false,
		},
		{
			name: "pod without a podgroup returns error",
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Namespace("ns1").Obj(),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns, err := validatePods(tt.pods)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePods() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				if ns != tt.wantNamespace {
					t.Errorf("validatePods() namespace = %v, want %v", ns, tt.wantNamespace)
				}
				for _, pod := range tt.pods {
					if pod.UID == "" {
						t.Errorf("expected pod %s UID to be populated, got empty", pod.Name)
					}
				}
			}
		})
	}
}

func TestBuildPodGroupHierarchy(t *testing.T) {
	featuregatetesting.SetFeatureGatesDuringTest(t, utilfeature.DefaultFeatureGate, featuregatetesting.FeatureOverrides{
		features.TopologyAwareWorkloadScheduling: true,
		features.GenericWorkload:                 true,
		features.CompositePodGroup:               true,
	})

	pg1 := st.MakePodGroup().Name("pg1").Namespace("default").Obj()
	pg1Pod1 := st.MakePod().Name("pod1").Namespace("default").PodGroupName("pg1").Obj()
	pg1Pod2 := st.MakePod().Name("pod2").Namespace("default").PodGroupName("pg1").Obj()

	rootCPG := st.MakeCompositePodGroup().Name("root-cpg").Namespace("default").Obj()

	cpg1 := st.MakeCompositePodGroup().Name("cpg1").Namespace("default").ParentCompositePodGroup("root-cpg").Obj()
	cpg1Leaf1 := st.MakePodGroup().Name("pg1-leaf1").Namespace("default").ParentCompositePodGroup("cpg1").Obj()
	cpg1Leaf2 := st.MakePodGroup().Name("pg1-leaf2").Namespace("default").ParentCompositePodGroup("cpg1").Obj()
	cpg1Leaf1Pod1 := st.MakePod().Name("pod1").Namespace("default").PodGroupName("pg1-leaf1").Obj()
	cpg1Leaf1Pod2 := st.MakePod().Name("pod2").Namespace("default").PodGroupName("pg1-leaf1").Obj()
	cpg1Leaf2Pod1 := st.MakePod().Name("pod3").Namespace("default").PodGroupName("pg1-leaf2").Obj()
	cpg1Leaf2Pod2 := st.MakePod().Name("pod4").Namespace("default").PodGroupName("pg1-leaf2").Obj()

	cpg2 := st.MakeCompositePodGroup().Name("cpg2").Namespace("default").ParentCompositePodGroup("root-cpg").Obj()
	cpg2Leaf1 := st.MakePodGroup().Name("pg2-leaf1").Namespace("default").ParentCompositePodGroup("cpg2").Obj()
	cpg2Leaf2 := st.MakePodGroup().Name("pg2-leaf2").Namespace("default").ParentCompositePodGroup("cpg2").Obj()
	cpg2Leaf1Pod1 := st.MakePod().Name("pod5").Namespace("default").PodGroupName("pg2-leaf1").Obj()
	cpg2Leaf1Pod2 := st.MakePod().Name("pod6").Namespace("default").PodGroupName("pg2-leaf1").Obj()
	cpg2Leaf2Pod1 := st.MakePod().Name("pod7").Namespace("default").PodGroupName("pg2-leaf2").Obj()
	cpg2Leaf2Pod2 := st.MakePod().Name("pod8").Namespace("default").PodGroupName("pg2-leaf2").Obj()

	// Cyclic CompositePodGroups
	cyclicCPG1 := st.MakeCompositePodGroup().Name("cyclic-cpg-1").Namespace("default").ParentCompositePodGroup("cyclic-cpg-2").Obj()
	cyclicCPG2 := st.MakeCompositePodGroup().Name("cyclic-cpg-2").Namespace("default").ParentCompositePodGroup("cyclic-cpg-1").Obj()
	cyclicCPG1Leaf := st.MakePodGroup().Name("cyclic-pg").Namespace("default").ParentCompositePodGroup("cyclic-cpg-1").Obj()
	cyclicPod := st.MakePod().Name("cyclic-pod").Namespace("default").PodGroupName("cyclic-pg").Obj()

	tests := []struct {
		name               string
		pods               []*v1.Pod
		podGroups          []*schedulingv1beta1.PodGroup
		compositePodGroups []*schedulingv1alpha3.CompositePodGroup
		wantRootName       string
		wantType           fwk.EntityKeyType
		wantChildren       int
		wantErr            bool
	}{
		{
			name: "single pod group hierarchy",
			pods: []*v1.Pod{
				pg1Pod1,
				pg1Pod2,
			},
			podGroups:    []*schedulingv1beta1.PodGroup{pg1},
			wantRootName: "pg1",
			wantType:     fwk.PodGroupKeyType,
			wantChildren: 0,
			wantErr:      false,
		},
		{
			name: "multi-level hierarchy",
			pods: []*v1.Pod{
				cpg1Leaf1Pod1,
				cpg1Leaf1Pod2,
				cpg1Leaf2Pod1,
				cpg1Leaf2Pod2,
				cpg2Leaf1Pod1,
				cpg2Leaf1Pod2,
				cpg2Leaf2Pod1,
				cpg2Leaf2Pod2,
			},
			podGroups:          []*schedulingv1beta1.PodGroup{cpg1Leaf1, cpg1Leaf2, cpg2Leaf1, cpg2Leaf2},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{rootCPG, cpg1, cpg2},
			wantRootName:       "root-cpg",
			wantType:           fwk.CompositePodGroupKeyType,
			wantChildren:       2,
			wantErr:            false,
		},
		{
			name:    "empty pod list error",
			wantErr: true,
		},
		{
			name: "missing pod group in snapshot",
			pods: []*v1.Pod{
				pg1Pod1,
				pg1Pod2,
			},
			wantErr: true,
		},
		{
			name: "missing parent composite pod group in snapshot",
			pods: []*v1.Pod{
				cpg1Leaf1Pod1,
				cpg1Leaf1Pod2,
				cpg1Leaf2Pod1,
				cpg1Leaf2Pod2,
			},
			podGroups: []*schedulingv1beta1.PodGroup{cpg1Leaf1, cpg1Leaf2},
			wantErr:   true,
		},
		{
			name: "cycle detected in hierarchy",
			pods: []*v1.Pod{
				cyclicPod,
			},
			podGroups:          []*schedulingv1beta1.PodGroup{cyclicCPG1Leaf},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{cyclicCPG1, cyclicCPG2},
			wantErr:            true,
		},
		{
			name: "disjoint hierarchies",
			pods: []*v1.Pod{
				cpg1Leaf1Pod1,
				cpg1Leaf1Pod2,
				cpg1Leaf2Pod1,
				cpg1Leaf2Pod2,
				cpg2Leaf1Pod1,
				cpg2Leaf1Pod2,
				cpg2Leaf2Pod1,
				cpg2Leaf2Pod2,
			},
			podGroups:          []*schedulingv1beta1.PodGroup{cpg1Leaf1, cpg1Leaf2, cpg2Leaf1, cpg2Leaf2},
			compositePodGroups: []*schedulingv1alpha3.CompositePodGroup{cpg1, cpg2},
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := cache.NewTestSnapshotWithCompositePodGroups(nil, nil, tt.podGroups, tt.compositePodGroups)

			root, err := BuildPodGroupHierarchy(snapshot, tt.pods)
			if (err != nil) != tt.wantErr {
				t.Errorf("BuildPodGroupHierarchy() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if root == nil || root.Name != tt.wantRootName || root.Type != tt.wantType {
					t.Errorf("BuildPodGroupHierarchy() root = %v, want name=%q type=%v", root, tt.wantRootName, tt.wantType)
				}
				if len(root.Children) != tt.wantChildren {
					t.Errorf("BuildPodGroupHierarchy() children count = %d, want %d", len(root.Children), tt.wantChildren)
				}
			}
		})
	}
}
