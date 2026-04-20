# Copyright The Kubernetes Authors.
# 
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# 
#     http://www.apache.org/licenses/LICENSE-2.0
# 
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

GO_CMD ?= go
GOLANGCI_LINT := $(GO_CMD) tool golangci-lint

.PHONY: all
all: build test lint

.PHONY: build
build:
	$(GO_CMD) build ./...

.PHONY: test
test:
	$(GO_CMD) test -v -race ./...

.PHONY: fmt
fmt:
	$(GOLANGCI_LINT) run --fix ./...

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run ./...

.PHONY: verify
verify: verify-modules lint test

.PHONY: verify-modules
verify-modules:
	$(GO_CMD) mod tidy
	git --no-pager diff --exit-code go.mod go.sum
