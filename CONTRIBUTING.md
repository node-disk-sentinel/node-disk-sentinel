# Contributing to Node Disk Sentinel

Thank you for contributing to Node Disk Sentinel. This project runs with
privileged access to Kubernetes nodes and disk devices. Changes must therefore
be small, reviewable, tested, and safe for existing installations.

## Before You Start

- Search existing issues and pull requests before starting work.
- Open an issue before proposing a substantial feature, behaviour change, or
  API change. Small documentation fixes do not require an issue.
- Keep each pull request focused on one logical change. Do not mix refactors,
  behaviour changes, and unrelated formatting changes.
- Describe the user-visible behaviour, tests performed, and any operational
  impact in the pull request. Use `Related to: #<issue-number>` when applicable.

## Developer Certificate of Origin

Every commit must include a Developer Certificate of Origin (DCO) sign-off.
The sign-off confirms that you wrote the contribution or have the right to
submit it under the project's license. See the
[Developer Certificate of Origin](https://developercertificate.org/) for the
full text.

Create signed-off commits with:

```sh
git commit -s -m "feat(discovery): support a new persistent disk identifier"
```

Git appends a line like this to the commit message:

```text
Signed-off-by: Your Name <your.email@example.com>
```

The name and email must identify the commit author. Cryptographically signed
commits (`git commit -S`) are encouraged, but do not replace the required DCO
sign-off (`git commit -s`).

## Commit Messages

All commits must follow the [Conventional Commits](https://www.conventionalcommits.org/)
specification:

```text
type(scope): short imperative description
```

Examples:

```text
feat(assessment): report NVMe media errors
fix(discovery): ignore NVMe partitions without a devtype
docs(helm): document the nds-system namespace
test(controller): cover missing disk reconciliation
```

Use one of these types:

- `feat`: a new user-visible capability
- `fix`: a user-visible bug fix
- `docs`: documentation-only change
- `test`: test-only change
- `refactor`: code restructuring without a behaviour change
- `perf`: performance improvement
- `build`: build system or dependency change
- `ci`: continuous integration or release automation change
- `chore`: maintenance that does not fit another type

Use a concise scope when useful. Common scopes are `api`, `assessment`,
`controller`, `discovery`, `helm`, `metrics`, `smartmontools`, and `ci`.

Mark incompatible changes with `!` after the type or scope and explain them in
a `BREAKING CHANGE:` footer:

```text
feat(api)!: rename disk health status

BREAKING CHANGE: Existing PhysicalDisk manifests must use the new field name.
```

## Development Workflow

The Makefile downloads pinned versions of `controller-gen` and `golangci-lint`
into `./bin/`; no global installation is required. Use the Go version declared
in `go.mod`.

```sh
make            # Generate CRDs, lint, test, and build
make test       # Run unit tests with race detection and coverage
make validate   # Run golangci-lint
make fix        # Format Go code and apply supported lint fixes
make generate   # Regenerate DeepCopy code and CRD manifests
make ci         # Run the CI checks, including generated-file drift detection
```

Run `make ci` before opening a pull request. It verifies that generated files
are current and that linting, tests, and the build pass.

## Code and API Guidelines

- Follow idiomatic Go and keep changes minimal. Run `make fix` to format code
  and apply supported linter fixes; do not manually reformat unrelated code.
- Add tests for bug fixes and new behaviour. Exercise failure paths when a
  change affects discovery, SMART collection, reconciliation, or status.
- Preserve backward compatibility for published `PhysicalDisk` API fields.
  Discuss breaking CRD, API-group, or label changes in an issue before coding.
- When changing files under `pkg/apis/`, run `make generate` and include the
  resulting DeepCopy and CRD changes in the pull request.
- Do not edit generated DeepCopy code or generated CRD schema by hand.

## Host and Security Requirements

The monitor accesses host `/dev`, `/sys`, and `/run/udev/data` and runs with
privileged Kubernetes permissions. Treat all host-facing input as untrusted.

- Do not invoke a shell to run `smartctl` or other commands. Pass arguments
  directly to process execution APIs.
- Do not wake standby disks unless the feature explicitly requires it.
- Keep uevent handling bounded and validate event data before acting on it.
- Apply least privilege to RBAC, mounts, capabilities, and network access.
- Do not add credentials, kubeconfigs, disk serial numbers, or other sensitive
  cluster data to commits, fixtures, logs, or pull request descriptions.

## Helm Charts and Documentation

- Keep chart defaults, CRD templates, RBAC, and README examples consistent.
- Use `.Release.Namespace` in namespaced Helm resources; do not hardcode a
  namespace in templates.
- Update user-facing documentation when installation, configuration, metrics,
  API fields, or operational requirements change.

## Review

Maintainers may request a rebase, tests, documentation, or a narrower change
before merging. Pull requests are merged only after CI passes and the DCO
sign-off is present on every commit.
