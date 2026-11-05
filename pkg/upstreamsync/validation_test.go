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

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
)

func TestValidatePercentage(t *testing.T) {
	path := field.NewPath("percentageOfNodesToScore")

	tests := []struct {
		name       string
		percentage *int32
		wantErrMsg string
	}{
		{
			name:       "nil is valid",
			percentage: nil,
		},
		{
			name:       "0 is valid",
			percentage: ptr.To[int32](0),
		},
		{
			name:       "50 is valid",
			percentage: ptr.To[int32](50),
		},
		{
			name:       "100 is valid",
			percentage: ptr.To[int32](100),
		},
		{
			name:       "negative value is invalid",
			percentage: ptr.To[int32](-1),
			wantErrMsg: "percentageOfNodesToScore: Invalid value: -1: not in valid range [0-100]",
		},
		{
			name:       "value above 100 is invalid",
			percentage: ptr.To[int32](101),
			wantErrMsg: "percentageOfNodesToScore: Invalid value: 101: not in valid range [0-100]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePercentage(path, tt.percentage)
			if err := verifyErr(tt.wantErrMsg, err); err != nil {
				t.Error(err)
			}
		})
	}
}
