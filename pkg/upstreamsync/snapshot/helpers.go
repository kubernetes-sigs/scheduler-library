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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

// addPodToNode adds a new pod to the specific node and returns the corresponding revert function
func addPodToNode(ctx context.Context, schedulerSnapshot *upstreamsync.MutatingSnapshot, pod *v1.Pod, nodeName string) (func(), error) {
	logger := klog.FromContext(ctx)
	clonedPod := pod.DeepCopy()
	clonedPod.Spec.NodeName = nodeName
	podInfo, err := framework.NewPodInfo(clonedPod)
	if err != nil {
		return nil, fmt.Errorf("failed to create pod info: %w", err)
	}
	err = schedulerSnapshot.AddPod(logger, podInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to add pod to snapshot: %w", err)
	}

	revertFn := func() {
		if _, err := removePodFromNode(ctx, schedulerSnapshot, podInfo.Pod); err != nil {
			logger.Error(err, "revert addPodToNode failed")
		}
	}

	return revertFn, nil
}

// removePodFromNode removes a pod from a specific node and returns the corresponding revert function.
func removePodFromNode(ctx context.Context, schedulerSnapshot *upstreamsync.MutatingSnapshot, pod *v1.Pod) (func(), error) {
	logger := klog.FromContext(ctx)
	podInfo, err := framework.NewPodInfo(pod.DeepCopy())
	if err != nil {
		return nil, fmt.Errorf("failed to create pod info: %w", err)
	}
	err = schedulerSnapshot.RemovePod(logger, podInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to remove pod from snapshot: %w", err)
	}

	revertFn := func() {
		if _, err := addPodToNode(ctx, schedulerSnapshot, pod, pod.Spec.NodeName); err != nil {
			logger.Error(err, "revert removePodFromNode failed")
		}
	}

	return revertFn, nil
}

// createPodFromTemplate creates a new v1.Pod based on the provided pod template spec and index.
// The pod's name is constructed as "<template-name>-<index>", and it is assigned a random UUID.
func createPodFromTemplate(template *v1.PodTemplateSpec, index int) *v1.Pod {
	podNamePrefix := template.Name
	if podNamePrefix == "" {
		podNamePrefix = "templated-pod"
	}
	ns := template.Namespace
	if ns == "" {
		ns = "default"
	}
	uid := types.UID(uuid.New().String())

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d-%s", podNamePrefix, index, uid),
			Namespace: ns,
			UID:       uid,
			Labels:    template.Labels,
		},
		Spec: template.Spec,
	}
}

// scheduleOnePod simulates a single scheduling cycle for a pod against the assumed placement.
// On success it returns the revert function undoing the changes the cycle made to the snapshot;
// on a scheduling failure the returned function is nil and the reason is carried by the result's
// Status. The pod is left untouched: it is up to the caller to reflect suggestedHost on it.
func scheduleOnePod(ctx context.Context, profiles *upstreamsync.ProfileMap, sched *upstreamsync.Scheduler, pod *v1.Pod) (*SchedulingResult, func(), error) {
	schedFramework, err := profiles.FrameworkForPod(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get framework for pod %s: %w", klog.KObj(pod), err)
	}

	gatingPlugin, status := runPreEnqueuePlugins(ctx, schedFramework, pod)
	if status.Code() == fwk.Error {
		return nil, nil, fmt.Errorf("plugin %q failed PreEnqueue: %w", gatingPlugin, status.AsError())
	}

	if status != nil {
		return &SchedulingResult{
			Status:       status,
			GatingPlugin: gatingPlugin,
			Pod:          pod,
		}, nil, nil
	}

	cycleState := framework.NewCycleState()
	cycleState.SetPodGroupSchedulingCycle(framework.NewCycleState())
	podInfo, err := framework.NewPodInfo(pod)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create pod info for pod %s: %w", klog.KObj(pod), err)
	}

	pendingPod := &upstreamsync.PendingPod{
		PodInfo:    podInfo,
		CycleState: cycleState,
	}
	algRes, revertFn := sched.SchedulePod(ctx, schedFramework, pendingPod)

	if !algRes.GetStatus().IsSuccess() {
		return schedulingResult(&algRes), nil, nil
	}

	return schedulingResult(&algRes), revertFn, nil
}

// runPreEnqueuePlugins runs the PreEnqueue plugins of the given framework for the pod.
// It returns the name of the plugin that gated the pod and its status, or ("", nil) if the pod
// passed every plugin.
func runPreEnqueuePlugins(ctx context.Context, schedFwk framework.Framework, pod *v1.Pod) (string, *fwk.Status) {
	for _, pl := range schedFwk.PreEnqueuePlugins() {
		status := pl.PreEnqueue(ctx, pod)

		if status.IsSuccess() {
			continue
		}

		if status.Code() == fwk.Error {
			utilruntime.HandleErrorWithContext(ctx, status.AsError(), "Unexpected error running PreEnqueue plugin", "pod", klog.KObj(pod), "plugin", pl.Name())
		} else {
			klog.FromContext(ctx).V(4).Info("Status after running PreEnqueue plugin", "pod", klog.KObj(pod), "plugin", pl.Name(), "status", status)
		}
		return pl.Name(), status
	}
	return "", nil
}

func schedulingResult(algRes *upstreamsync.AlgorithmResult) *SchedulingResult {
	return &SchedulingResult{
		Pod:              algRes.GetPod(),
		Status:           algRes.GetStatus(),
		SelectedNodeName: algRes.GetNodeName(),
	}
}
