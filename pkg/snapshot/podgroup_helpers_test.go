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
	testutils "sigs.k8s.io/scheduler-library/pkg/upstreamsync/testutils"
)

func TestIsPodGroupMember(t *testing.T) {
	tests := []struct {
		name string
		pod  *v1.Pod
		want bool
	}{
		{
			name: "pod without scheduling group",
			pod:  st.MakePod().Name("pod-no-sg").Obj(),
			want: false,
		},
		{
			name: "pod with empty pod group name",
			pod:  st.MakePod().Name("pod-empty-sg").PodGroupName("").Obj(),
			want: false,
		},
		{
			name: "valid pod with pod group name",
			pod:  st.MakePod().Name("pod-valid").PodGroupName("pg-1").Obj(),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPodGroupMember(tt.pod)
			if got != tt.want {
				t.Errorf("isPodGroupMember() = %v, want %v", got, tt.want)
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
			name: "nil pod in list returns error",
			pods: []*v1.Pod{
				nil,
			},
			wantErr: true,
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

	pg1 := testutils.MakeBasicPodGroup("pg1", "")
	pg1Pod1 := testutils.MakePod("pod1", "pg1", "")
	pg1Pod2 := testutils.MakePod("pod2", "pg1", "")

	rootCPG := testutils.MakeBasicCompositePodGroup("root-cpg", "")

	cpg1 := testutils.MakeBasicCompositePodGroup("cpg1", "root-cpg")
	cpg1Leaf1 := testutils.MakeBasicPodGroup("pg1-leaf1", "cpg1")
	cpg1Leaf2 := testutils.MakeBasicPodGroup("pg1-leaf2", "cpg1")
	cpg1Leaf1Pod1 := testutils.MakePod("pod1", "pg1-leaf1", "")
	cpg1Leaf1Pod2 := testutils.MakePod("pod2", "pg1-leaf1", "")
	cpg1Leaf2Pod1 := testutils.MakePod("pod3", "pg1-leaf2", "")
	cpg1Leaf2Pod2 := testutils.MakePod("pod4", "pg1-leaf2", "")

	cpg2 := testutils.MakeBasicCompositePodGroup("cpg2", "root-cpg")
	cpg2Leaf1 := testutils.MakeBasicPodGroup("pg2-leaf1", "cpg2")
	cpg2Leaf2 := testutils.MakeBasicPodGroup("pg2-leaf2", "cpg2")
	cpg2Leaf1Pod1 := testutils.MakePod("pod5", "pg2-leaf1", "")
	cpg2Leaf1Pod2 := testutils.MakePod("pod6", "pg2-leaf1", "")
	cpg2Leaf2Pod1 := testutils.MakePod("pod7", "pg2-leaf2", "")
	cpg2Leaf2Pod2 := testutils.MakePod("pod8", "pg2-leaf2", "")

	// Cyclic CompositePodGroups
	cyclicCPG1 := testutils.MakeBasicCompositePodGroup("cyclic-cpg-1", "cyclic-cpg-2")
	cyclicCPG2 := testutils.MakeBasicCompositePodGroup("cyclic-cpg-2", "cyclic-cpg-1")
	cyclicCPG1Leaf := testutils.MakeBasicPodGroup("cyclic-pg", "cyclic-cpg-1")
	cyclicPod := testutils.MakePod("cyclic-pod", "cyclic-pg", "")

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

			root, err := buildPodGroupHierarchy(snapshot, tt.pods)
			if (err != nil) != tt.wantErr {
				t.Errorf("buildPodGroupHierarchy() error = %v, wantErr %v", err, tt.wantErr)
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
