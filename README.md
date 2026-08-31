# scheduler-library

The Scheduler Library provides a mechanism for performing in-memory scheduling simulations by leveraging the existing Kubernetes scheduler codebase. It is designed to enable "what-if" scenarios—such as preemption testing and workload feasibility analysis—without mutating the actual cluster state.

## Overview

This library allows you to create a frozen, in-memory view of a cluster (`ClusterSnapshot`) and simulate scheduling decisions. It is particularly useful for controllers like Kueue that need to evaluate complex scheduling permutations, preemption sets, and resource availability before committing to actions on a live cluster.

## Core concepts

* `ClusterSnapshot`: Represents a state of the cluster frozen in time. All operations are in-memory and non-destructive. You can schedule pods, preempt workloads, and add/remove nodes within a snapshot.
* `ClusterState`: Reflects the real, runtime state of the cluster. It serves as the primary source for initializing snapshots.
* `SchedulingSimulator`: The primary interface for creating snapshots and managing cluster states.

## Entry points

Everything a consumer needs is reachable from the [`pkg/simulator`](pkg/simulator) package, which documents the full flow:

1. `simulator.NewReadonlyClient(restConfig)` — the only client type the library accepts; its transport rejects mutating requests.
2. `simulator.NewSchedulingSimulator(ctx, cfg, client, informerFactory)` — created once and reused.
3. `SchedulingSimulator.NewClusterState(ctx)` (live cluster state) or `SchedulingSimulator.NewClusterSnapshot(ctx, pods, nodes)` (explicit state).
4. `ClusterSnapshot.MakePlacement`, `CanSchedulePod`, `SchedulePods`, `SchedulePodsByTemplate`, `PreemptPods`, `Unpreempt` and `Transaction` — the simulation methods.

The remaining packages are implementation detail: `pkg/upstreamsync` holds logic duplicated from (or destined for) the upstream kube-scheduler, and `pkg/framework` wires the upstream scheduler framework for in-memory use.

## Key capabilities

* **Transaction Support**: Execute sequences of mutations (e.g., preemptions, scheduling) with the ability to commit or revert changes. This supports complex branching scenarios during simulation.
* **Feasibility Checking**: Perform efficient checks to see if pods can be scheduled on specific nodes (SchedulePods, CanSchedulePod) without affecting the snapshot state.
* **Minimalism**: Designed to reuse core Kubernetes scheduler logic, minimizing library-specific implementation.

## Out-of-tree plugins

Consumers that run a scheduler with out-of-tree Scheduling Framework plugins must give the
library the same plugin registry, otherwise the simulation diverges from the real scheduling
decision. Pass your `frameworkruntime.Registry` to `simulator.NewSchedulingSimulator`:

```go
sim, err := simulator.NewSchedulingSimulator(ctx, cfg, readonlyClient, informerFactory,
	upstreamsync.WithFrameworkOutOfTreeRegistry(frameworkruntime.Registry{
		"MyPlugin": myplugin.New,
	}),
)
```

The registry is merged into the in-tree one exactly as `scheduler.WithFrameworkOutOfTreeRegistry`
does upstream, so a name that collides with an in-tree plugin is an error. The registry is not
copied — do not mutate it after passing it in.

### Limitations

* **Only a subset of extension points is executed.** The simulation only runs the extension points
  necessary for in-memory placement simulation. The exact list of extension points executed is defined
  in [`pkg/upstreamsync/scheduler.go`](pkg/upstreamsync/scheduler.go) (see `Scheduler.SchedulePod`).
  Note that `QueueSort` and `Bind` plugins must still be present in the profile — the framework
  refuses to build without them — but they are not used by the simulation.
* **`handle.KubeConfig()` is available, but read-only.** It is populated automatically from the
  `ReadonlyClient` given to the simulator and cannot be overridden with WithKubeConfig at the 
  simulator level, so plugin factories doing `myclientset.NewForConfigOrDie(handle.KubeConfig())` work. 
  Clients built from it reject POST/PUT/PATCH/DELETE at the transport level. A plugin that builds 
  its own config, e.g. via `rest.InClusterConfig()`, bypasses this protection.
* **The data source decides whether the result is correct.** A plugin that reads cluster state
  through `handle.SnapshotSharedLister()` sees the simulated state. A plugin that reads it
  through `handle.SharedInformerFactory()` or its own clients — typical for CRD-based plugins such
  as the ones in kubernetes-sigs/scheduler-plugins — sees the **real** cluster: hypothetical pods
  of the simulation do not exist for it, and its verdict will differ from the real scheduler's.

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
