# Contributing Guidelines

Welcome to Kubernetes. We are excited about the prospect of you joining our [community](https://git.k8s.io/community)! The Kubernetes community abides by the CNCF [code of conduct](code-of-conduct.md). Here is an excerpt:

_As contributors and maintainers of this project, and in the interest of fostering an open and welcoming community, we pledge to respect all people who contribute through reporting issues, posting feature requests, updating documentation, submitting pull requests or patches, and other activities._

## Getting Started

We have full documentation on how to get started contributing here:

<!---
If your repo has certain guidelines for contribution, put them here ahead of the general k8s resources
-->

- [Contributor License Agreement](https://git.k8s.io/community/CLA.md) - Kubernetes projects require that you sign a Contributor License Agreement (CLA) before we can accept your pull requests
- [Kubernetes Contributor Guide](https://k8s.dev/guide) - Main contributor documentation, or you can just jump directly to the [contributing page](https://k8s.dev/docs/guide/contributing/)
- [Contributor Cheat Sheet](https://k8s.dev/cheatsheet) - Common resources for existing developers
- [SIG Scheduling Contributor's Guide](https://git.k8s.io/community/sig-scheduling/CONTRIBUTING.md) - Guidelines specific to SIG Scheduling.

## Mentorship

- [Mentoring Initiatives](https://k8s.dev/community/mentoring) - We have a diverse set of mentorship programs available that are always looking for volunteers!

## Contact Information

- [Slack channel](https://kubernetes.slack.com/messages/sig-scheduling)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/sig-scheduling)

## Development philosophy

Our long-term goal is to migrate this simulation logic into the core Kubernetes (k/k) codebase. To ensure a seamless transition, we maintain a strict development process that prioritizes upstreaming improvements.

### Explicit packaging for borrowed logic

Any logic duplicated from the Kubernetes scheduler (e.g. to export internal code) must be placed in a dedicated package named `/pkg/upstream_sync`. Treat this code as "temporary debt" and include a comment in the code with a link to the upstream PR that will remove this code.

### Synchronous upstreaming

We discourage an “eventually” mindset. Any change to core scheduling logic in this library should trigger a parallel contribution to the upstream Kubernetes scheduler.

Contributors should ideally open a PR in the upstream scheduler first, or simultaneously with the library PR.

Reviewers may reject PRs that add complex scheduling logic to the library if that logic is appropriate for the upstream framework.

### Testing

We want to avoid changes in upstream causing breakages in the library.

If you change the scheduler code to use it in the library, make sure to cover it with a test and make it clear that it's used by the library.

### Short-term exceptions

We recognize that the library will undergo several iterations before stabilizing (e.g., during performance experimentation or API definition for integration with Kueue). If following these rules strictly causes significant overhead, an exception may be granted by repository owners to delay actions like creating a scheduler PR. These exceptions should be tracked via a GitHub issue and must be lifted once integration is complete.