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

package simulator

import (
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"sigs.k8s.io/scheduler-library/pkg/upstreamsync"
)

// ValidateKubeSchedulerConfiguration validates the fields of KubeSchedulerConfiguration
// used by the simulator.
func ValidateKubeSchedulerConfiguration(cc *schedulerapi.KubeSchedulerConfiguration) utilerrors.Aggregate {
	var errs []error
	profilesPath := field.NewPath("profiles")
	for i := range cc.Profiles {
		profile := &cc.Profiles[i]
		path := profilesPath.Index(i)
		errs = append(errs, validateKubeSchedulerProfile(path, profile)...)
	}
	errs = append(errs, upstreamsync.ValidatePercentage(field.NewPath("percentageOfNodesToScore"), cc.PercentageOfNodesToScore))
	return utilerrors.Flatten(utilerrors.NewAggregate(errs))
}

func validateKubeSchedulerProfile(path *field.Path, profile *schedulerapi.KubeSchedulerProfile) []error {
	var errs []error
	errs = append(errs, upstreamsync.ValidatePercentage(path.Child("percentageOfNodesToScore"), profile.PercentageOfNodesToScore))
	return errs
}
