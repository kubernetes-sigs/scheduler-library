# scheduler-library

The Scheduler Library provides a mechanism for performing in-memory scheduling simulations by leveraging the existing Kubernetes scheduler codebase. It is designed to enable "what-if" scenarios—such as preemption testing and workload feasibility analysis—without mutating the actual cluster state.

## Overview

This library allows you to create a frozen, in-memory view of a cluster (`ClusterSnapshot`) and simulate scheduling decisions. It is particularly useful for controllers like Kueue that need to evaluate complex scheduling permutations, preemption sets, and resource availability before committing to actions on a live cluster.

## Core concepts

* `ClusterSnapshot`: Represents a state of the cluster frozen in time. All operations are in-memory and non-destructive. You can schedule pods, preempt workloads, and add/remove nodes within a snapshot.
* `ClusterState`: Reflects the real, runtime state of the cluster. It serves as the primary source for initializing snapshots.
* `SchedulingSimulator`: The primary interface for creating snapshots and managing cluster states.

## Key capabilities

* **Transaction Support**: Execute sequences of mutations (e.g., preemptions, scheduling) with the ability to commit or revert changes. This supports complex branching scenarios during simulation.
* **Feasibility Checking**: Perform efficient checks to see if pods can be scheduled on specific nodes (SchedulePods, CanSchedulePod) without affecting the snapshot state.
* **Minimalism**: Designed to reuse core Kubernetes scheduler logic, minimizing library-specific implementation.

## Compatibility and versioning

* **Dependencies**: The library directly imports k8s.io/kubernetes.
* **Version Skew**: The library is designed to align with the official Kubernetes release policy, aiming to support three minor releases back.
* **Feature Gates**: The library supports emulating feature gates for specific Kubernetes versions, allowing for consistent scheduling results even when the library version differs from the cluster version.

The library will continue to function in case of a bigger version skew with the k8s version, however the simulation results may diverge from the actual scheduling decisions.

## Development philosophy

The library prioritizes alignment with the upstream Kubernetes scheduler. Our long-term goal is to migrate simulation logic into the core Kubernetes codebase. Please refer to our CONTRIBUTING.md for detailed guidelines on how to contribute and keep this library in sync with upstream scheduler improvements.

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/sig-scheduling)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-scheduling)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).
