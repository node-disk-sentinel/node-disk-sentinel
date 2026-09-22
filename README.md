# Node Disk Sentinel

> **Disk failure is inevitable. Downtime isn’t.**  
> Predict and handle failing Kubernetes node disks before workloads are impacted.

Continuous SMART assessment for every node disk, with Kubernetes resources and Prometheus metrics for the response workflow you already use.

## Architecture

- A single daemon binary runs on each node; no `smartd` daemon or separate
  controller process is required.
- The monitor reads udev's runtime database directly from `/run/udev/data` and
  uses stable device paths where available. It operates on a strict 1:1 mapping
  between discovered host block devices (`/dev/sd*`, `/dev/nvme*n*`) and
  cluster-scoped `PhysicalDisk` resources.
- It polls disks every 10 minutes by default and listens for udev-processed
  Netlink add, change, online, remove, and offline events. Add/change bursts
  are coalesced before a full inventory refresh.
- `hostNetwork: true` is required so the monitor can receive host udev Netlink
  broadcasts. The DaemonSet also mounts the host `/dev`, `/run/udev/data`, and
  `/sys` paths and runs privileged to invoke `smartctl` against host devices.

## Status Model

`PhysicalDisk.status.health` is one of `Good`, `SectorErrors`,
`ExcessiveSectorErrors`, `AttributeFailingNow`, `AttributeFailedInPast`,
`SelfAssessmentFailed`, or `Unknown`.

### Health Assessment Cascade

Drive health is assessed using a deterministic, prioritized multi-protocol severity cascade (first match wins):

1. **ATA Disks** (derived from the severity cascade of `libatasmart`):
   - `SelfAssessmentFailed`: Drive overall-health test failed (`smart_status.passed == false` or smartctl exit bit 3).
   - `ExcessiveSectorErrors`: Reallocated (ATA 5) or pending (ATA 197) sectors breached manufacturer threshold (`when_failed == "failing_now"`).
   - `AttributeFailingNow`: Any other pre-failure attribute currently failing its manufacturer threshold.
   - `SectorErrors`: Bad sectors exist in raw count (ATA 5 or ATA 197 `> 0`), but normalized attributes have not breached threshold.
   - `AttributeFailedInPast`: An attribute previously dropped below threshold in the past (`when_failed == "in_the_past"`).
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

The resource reports two conditions:

- `Degraded` records the evaluated disk health. Its reason always matches
  `status.health`.
- `DataCollected` records whether disk data was obtained, with reasons
  `Succeeded`, `Failed`, `SkippedStandby`, `DiskMissing`, and
  `SmartUnsupported`.

`status.lastDataCollectedTime` changes only after successful collection.

`status.info.rotational` indicates whether the medium is rotational (HDD: `true`, SSD/NVMe: `false`).

`status.info.canonicalPath` stores the kernel device path reported by udev (for
example, `/dev/sda` or `/dev/nvme0n1`). `status.info.path` remains the preferred
persistent path used by the collector.

`status.telemetry` captures a compact operational snapshot from the last
successful collection (`temperatureCelsius`, `powerOnHours`, `powerCycleCount`,
ATA sector counts, NVMe percentage used, available spare, and media errors).
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

Every metric includes `node`, `disk`, and `device` labels alongside standard `smartctl_exporter` labels. All readings of a
detached disk are automatically pruned to prevent stale metric export.

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

Each rule is a comma-separated string of exact matches (`node`, `name`, `vendor`, `model`, `serial`, `wwn`, `bus`). `node` is optional and limits the rule to that Kubernetes Node; all supplied fields must match. Excluded devices are neither probed nor published as `PhysicalDisk` resources, and any matching existing resources are automatically removed. See the [FAQ](FAQ.md) for more examples.

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
make package
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
