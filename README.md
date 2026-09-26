# Node Disk Sentinel

> **Disk failure is inevitable. Downtime isn’t.**  
> Predict and handle failing Kubernetes node disks before workloads are impacted.

Continuous SMART assessment for every node disk, with Kubernetes resources and Prometheus metrics for the response workflow you already use.

## Architecture

- A single daemon binary runs on each node; no `smartd` daemon or separate
  controller process is required.
- The monitor lists whole disks from `/sys/block` and reads udev's runtime
  database from `/run/udev/data` for their identity and stable device paths. It
  operates on a strict 1:1 mapping between discovered host block devices
  (`/dev/sd*`, `/dev/nvme*n*`) and cluster-scoped `PhysicalDisk` resources.
- It polls disks every 10 minutes by default and listens for udev-processed
  Netlink events. Adding or removing a disk triggers an inventory refresh;
  bursts of events are coalesced. Events are only a hint: the refresh always
  re-reads sysfs and the udev database, and it also runs whenever the listener
  (re)connects, so no change is missed.
- It streams Linux kernel messages (`/dev/kmsg`) in real time to instantly detect
  hardware and block layer errors (`I/O error, dev ...`, SCSI sense errors, NVMe
  timeouts). Errors are buffered in memory. A fixed delay after the first one
  (`--kmsg-debounce=5s`) lets kernel error recovery (SCSI aborts/resets) settle;
  then a SMART re-scan runs, diagnostic findings are attached, and the disk health
  is degraded (`KernelErrors`) without waiting for the next poll.
- `hostNetwork: true` is required so the monitor can receive host udev Netlink
  broadcasts. The DaemonSet also mounts the host `/dev`, `/run/udev/data`, and
  `/sys` paths and runs privileged to invoke `smartctl` against host devices.

### Design Principles & Separation of Concerns

Node Disk Sentinel strictly separates operational state from time-series telemetry:

- **Kubernetes CRs (`PhysicalDisk`) are for orchestration and state:** Custom Resources store hardware metadata, current evaluated health, and compact operational conditions. They intentionally never store historical data. This enables cluster-native automation and `kubectl` visibility without creating runtime dependencies on external monitoring APIs.
- **Prometheus is for time series:** Continuous telemetry, wear-level trend analysis, downsampling, and Alertmanager integrations are handled via the built-in Prometheus exporter (`:8080/metrics`), which is fully compatible with `prometheus-community/smartctl_exporter` and existing Grafana dashboards.
- **Minimal etcd footprint:** With a default 10-minute polling cycle (`--poll-interval=10m`), etcd write load remains negligible even in large clusters (e.g. ~1 write/sec across a 100-node cluster with 600 drives).

## Status Model

`PhysicalDisk.status.health` is one of `Good`, `SectorErrors`,
`ExcessiveSectorErrors`, `AttributeFailingNow`, `AttributeFailedInPast`,
`SelfAssessmentFailed`, `KernelErrors`, or `Unknown`.

### Health Assessment Cascade

Drive health is assessed using a deterministic, prioritized multi-protocol severity cascade (first match wins). The evaluation model is grounded in formal storage specifications, empirical failure research, and proven concepts from the Linux storage ecosystem:

1. **ATA Disks** (incorporating tiered severity concepts inspired by `libatasmart`):
   - `SelfAssessmentFailed`: Drive overall-health test failed (`smart_status.passed == false` or smartctl exit bit 3).
   - `ExcessiveSectorErrors`: Reallocated (ATA 5) or pending (ATA 197) sectors breached manufacturer threshold (`when_failed == "now"`).
   - `AttributeFailingNow`: Any other attribute currently at or below its manufacturer threshold (`when_failed == "now"`, or smartctl exit bit 4 for a pre-failure attribute).
   - `SectorErrors`: Bad sectors exist in raw count (ATA 5 or ATA 197 `> 0`), but normalized attributes have not breached threshold.
   - `AttributeFailedInPast`: An attribute previously dropped below threshold in the past (`when_failed == "past"`).
   - `Good`: All attributes within design parameters and zero sector errors observed.

2. **NVMe Drives** (derived from the NVM Express Base Specification, SMART / Health Information Log):
   - `SelfAssessmentFailed`: Device self-assessment failed or controller `CriticalWarning > 0` (spare below threshold, temperature, reliability degraded, read-only mode, or volatile memory backup failed).
   - `AttributeFailingNow`: `AvailableSpare < AvailableSpareThreshold` (spare depleted) or `PercentageUsed >= 100` (endurance exhausted).
   - `SectorErrors`: `MediaErrors > 0` (unrecovered data integrity errors; the functional NVMe equivalent of bad sectors).
   - `Good`: Zero critical warnings, healthy spare capacity, and zero media errors.

3. **SCSI / SAS Disks** (derived from the SCSI Primary Commands / SPC standard):
   - `SelfAssessmentFailed`: Self-assessment reported failure.
   - `SectorErrors`: `TotalUncorrectedErrors > 0` in SCSI read or write error counter logs.
   - `Good`: Zero uncorrected read/write errors.

4. **Kernel Errors** (all protocols, from `/dev/kmsg`): A block layer, SCSI or NVMe error logged for the disk attaches a `KERNEL_*` finding. The finding survives later SMART collections: a disk that SMART reports as `Good` or `AttributeFailedInPast` stays at `KernelErrors` and does not heal until the drive is replaced. Any more severe status from the cascade above takes precedence.

The resource reports two conditions:

- `Degraded` records the evaluated disk health. Its reason always matches
  `status.health`.
- `DataCollected` records whether disk data was obtained, with reasons
  `Succeeded`, `Failed`, `SkippedStandby`, `DiskMissing`, and
  `SmartUnsupported`.

`status.lastDataCollectedTime` changes only after successful collection.

`status.info.rotational` indicates whether the medium is rotational (HDD: `true`, SSD/NVMe: `false`).

`status.info.canonicalPath` stores the kernel device path (for example,
`/dev/sda` or `/dev/nvme0n1`). `status.info.path` remains the preferred
persistent path used by the collector.

`status.telemetry` captures a compact operational snapshot from the last
successful collection (`temperatureCelsius`, `powerOnHours`, `powerCycleCount`,
ATA sector counts, NVMe percentage used, available spare, critical warning, and media errors).
A failed collection preserves the previous telemetry snapshot.

A failed collection describes the collection, not the hardware. The last known
health is therefore preserved, and only a disk that was never assessed reports
`Unknown`. Consequently, `kubectl get physicaldisks` can display `HEALTH=Good`
alongside `COLLECTION=DiskMissing` (or `Failed` / `SkippedStandby`): `HEALTH`
indicates the last evaluated hardware health, while `COLLECTION` shows that data
could not currently be collected (e.g. because the drive was detached). Check
`status.lastDataCollectedTime` to see when the last successful assessment occurred.
Kubernetes events are emitted on state transitions rather than on every poll.

## Prometheus Metrics

The monitor exports Prometheus metrics on `:8080/metrics` using the standard
`smartctl_*` namespace, fully compatible with `prometheus-community/smartctl_exporter`
and community Grafana dashboards (such as dashboard `22604`):

- `smartctl_version`: smartctl version and metadata (compatible with dashboard template variables).
- `smartctl_device`: Device hardware metadata and identifiers (`interface`, `protocol`, `model_name`, `serial_number`, `firmware_version`, etc.).
- `smartctl_device_smart_status`: SMART overall-health self-assessment test result (`1` = passed, `0` = failed).
- `smartctl_device_temperature`: Current temperature in Celsius.
- `smartctl_device_power_on_seconds`: Lifetime power-on time in seconds.
- `smartctl_device_power_cycle_count`: Power-on/off cycle count.
- `smartctl_device_percentage_used`: Subsystem life used percentage (NVMe).
- `smartctl_device_available_spare`: Remaining spare capacity percentage (NVMe).
- `smartctl_device_available_spare_threshold`: Spare capacity warning threshold (NVMe).
- `smartctl_device_critical_warning`: Critical warning bitmask (NVMe).
- `smartctl_device_media_errors`: Unrecovered data integrity errors (NVMe).
- `smartctl_device_num_err_log_entries`: Error information log count (NVMe).
- `smartctl_device_attribute`: SMART attributes (raw, value, worst, threshold).
- `smartctl_device_health_status`: Evaluated disk health status gauge (`Good`, `SectorErrors`, etc.).
- `smartctl_device_collection_success`: Last SMART data collection result (`1` = success, `0` = failure).
- `node_disk_sentinel_kernel_errors_total`: Counter tracking kernel-reported storage errors (block layer I/O, SCSI sense, NVMe controller) by operation (`WRITE`, `READ`, etc.).

Every metric except `smartctl_version` includes `node`, `disk`, and `device` labels alongside standard `smartctl_exporter` labels
(`smartctl_version` carries only `node`). All readings of a detached disk are automatically pruned to prevent stale metric export.

Prometheus metrics export can be disabled via the `--metrics-enabled=false` flag (or `metrics.enabled: false` in Helm). When disabled, the HTTP server continues serving `/healthz` and `/readyz` for health and readiness probes while returning 404 for `/metrics`.

## Installation

Deployment is split into two Helm charts. Install the CRD chart first,
then install the monitor DaemonSet chart. Helm 3 and cluster-admin permissions
are required because `PhysicalDisk` is cluster-scoped and the monitor needs a
`ClusterRole`.

The monitor runs privileged, uses the host network namespace, and mounts the
host `/dev`, `/run/udev/data`, and `/sys` paths. Review these requirements
before deploying it to a production cluster.

### Install via Helm (OCI Registry)

The Helm charts are published as OCI packages to the GitHub Container Registry. Install the CRD chart first, followed by the monitor chart:

```sh
helm upgrade --install node-disk-sentinel-crd \
  oci://ghcr.io/node-disk-sentinel/charts/node-disk-sentinel-crd \
  --version <version> \
  --namespace nds-system \
  --create-namespace

helm upgrade --install node-disk-sentinel \
  oci://ghcr.io/node-disk-sentinel/charts/node-disk-sentinel \
  --version <version> \
  --namespace nds-system
```

### Install From a Checkout

```sh
git clone https://github.com/node-disk-sentinel/node-disk-sentinel.git
cd node-disk-sentinel

helm upgrade --install node-disk-sentinel-crd \
  ./charts/node-disk-sentinel-crd \
  --namespace nds-system \
  --create-namespace

helm upgrade --install node-disk-sentinel \
  ./charts/node-disk-sentinel \
  --namespace nds-system
```

The default monitor image is `ghcr.io/node-disk-sentinel/node-disk-sentinel:0.1.0`. Set a
different image and tag when deploying a locally built or unreleased image:

```sh
helm upgrade --install node-disk-sentinel \
  ./charts/node-disk-sentinel \
  --namespace nds-system \
  --set image.repository=ghcr.io/node-disk-sentinel/node-disk-sentinel \
  --set image.tag=<version-or-tag>
```

### Verify

```sh
kubectl get crd physicaldisks.node-disk-sentinel.org
kubectl get daemonset -n nds-system node-disk-sentinel
kubectl get pods -n nds-system -l app.kubernetes.io/name=node-disk-sentinel
kubectl get physicaldisks
```

Inspect the reported status and collection condition of a disk:

```sh
kubectl get physicaldisks
kubectl describe physicaldisk <physical-disk-name>
```

### Customizing smartctl Options for a Disk

Some disks (e.g. behind USB enclosures, RAID controllers, or with non-standard
firmware) require custom `smartctl` arguments such as device-type overrides
(`-d sat`, `-d megaraid,0`) or tolerance options (`-T permissive`).

You can configure additional command-line flags on any `PhysicalDisk` via
`spec.smartmontools.smartctl.extraCmdArgs`:

```yaml
apiVersion: node-disk-sentinel.org/v1alpha1
kind: PhysicalDisk
metadata:
  name: node-1-sda
spec:
  nodeName: node-1
  smartmontools:
    smartctl:
      extraCmdArgs: "-d sat -T permissive"
```

Or patch an existing disk directly with `kubectl`:

```sh
kubectl patch physicaldisk <disk-name> --type=merge -p \
  '{"spec":{"smartmontools":{"smartctl":{"extraCmdArgs":"-d sat"}}}}'
```

The monitor watches for `spec` changes and re-probes the disk immediately,
without waiting for the next periodic poll cycle.

### Hardware RAID and 1:1 Device Mapping

`node-disk-sentinel` relies on a strict 1:1 mapping between OS block devices and `PhysicalDisk` resources. Hardware RAID logical volumes (e.g. HPE Smart Array, MegaRAID, Dell PERC) hide member drives from the operating system and prevent independent per-disk monitoring.

**Recommendation:** Run storage controllers in **HBA / Pass-Through mode** so each physical drive is exposed directly to the OS. For controller overrides, passthrough flags, and architectural details, see the [FAQ](FAQ.md).

### Excluding Disks From Discovery

Use `discovery.exclude` to omit devices (e.g. RAID logical volumes or non-monitored disks) from discovery:

```yaml
discovery:
  exclude:
    - "vendor=HP,model=LOGICAL_VOLUME"
    - "name=sdn"
    - "node=worker-03,serial=ABC123"
```

Each rule is a comma-separated string of exact matches (`node`, `name`, `vendor`, `model`, `serial`, `wwn`, `bus`; `wwn` is compared case-insensitively). `node` is optional and limits the rule to that Kubernetes Node; all supplied fields must match. Excluded devices are neither probed nor published as `PhysicalDisk` resources, and any matching existing resources are automatically removed. See the [FAQ](FAQ.md) for more examples.

### Upgrade And Uninstall

Upgrade the CRD chart before the monitor chart:

```sh
helm upgrade node-disk-sentinel-crd ./charts/node-disk-sentinel-crd \
  --namespace nds-system
helm upgrade node-disk-sentinel ./charts/node-disk-sentinel \
  --namespace nds-system
```

The CRD chart sets `helm.sh/resource-policy: keep`. Uninstalling the monitor
removes the DaemonSet but retains the CRD and existing `PhysicalDisk` objects:

```sh
helm uninstall node-disk-sentinel --namespace nds-system
helm uninstall node-disk-sentinel-crd --namespace nds-system
```

Delete the retained API objects explicitly only when they are no longer needed:

```sh
kubectl delete physicaldisks --all
kubectl delete crd physicaldisks.node-disk-sentinel.org
```

## Development

```sh
make build      # Compile daemon binary
make test       # Run unit tests with race detection and coverage
make validate   # Run golangci-lint
make fix        # Format and auto-fix lint findings
make generate   # Regenerate DeepCopy code and CRDs
make package    # Build the container image
make save       # Export the container image as a tar archive
```

The default image reference is `ghcr.io/node-disk-sentinel/node-disk-sentinel`.

### Local Kubernetes and Monitoring Setup

[`hack/`](hack/README.md) provides a local developer setup based on Kind. It
builds and deploys `node-disk-sentinel` to a local Kubernetes cluster and
includes a Docker Compose monitoring stack with Prometheus and a pre-provisioned
Grafana dashboard.

```sh
cd hack
make cluster-up
make deploy
make monitoring-up
```

### Versioning and Releases

Build versions are derived from Git. An exact release tag is used unchanged;
development builds use Git's describe format, for example
`v0.2.0-4-g442c566abc12`. The same version identifies the locally built binary
and container image:

```sh
make build package
./bin/node-disk-sentinel --version
```

Create releases by pushing an annotated SemVer tag:

```sh
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

The release workflow publishes multi-architecture container images and both
Helm charts to GitHub Container Registry, then creates a GitHub release with
the chart archives and a rendered installation manifest. Pre-release tags such
as `v0.2.0-rc1` follow the same flow but are marked as pre-releases and never
update the `latest` image tag.
