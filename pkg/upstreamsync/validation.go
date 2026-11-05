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
	"k8s.io/apimachinery/pkg/util/validation/field"
)

/*

Extracted from kubernetes/kubernetes/pkg/scheduler/apis/config/validation/validation.go

Deviations from upstream validation.go are marked with the "UPSTREAM-DIFF:" prefix,
see pkg/upstreamsync/doc.go.

*/

// ValidatePercentage checks if percentage is in a range from 0 to 100.
//
// UPSTREAM-DIFF: copied verbatim from Kubernetes 1.38 validation.go (ahead of v1.37.0).
func ValidatePercentage(path *field.Path, percentage *int32) error {
	if percentage != nil {
		if *percentage < 0 || *percentage > 100 {
			return field.Invalid(path, *percentage, "not in valid range [0-100]")
		}
	}
	return nil
}
