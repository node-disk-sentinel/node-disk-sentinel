# Frequently Asked Questions (FAQ)

### Can I monitor disks behind a Hardware RAID controller?

Node Disk Sentinel requires a strict **1:1 mapping** between kernel block devices (`/dev/sd*`, `/dev/nvme*n*`) discovered via udev and Kubernetes `PhysicalDisk` resources.

Hardware RAID controllers typically combine multiple physical disks into virtual logical volumes (e.g. `/dev/sda` with model `LOGICAL_VOLUME`), hiding the underlying physical drives from the operating system:
- `smartctl` cannot read native SMART health from virtual RAID volumes directly (failing with errors such as `requires option '-d cciss,N'`).
- Controller passthrough overrides can only query a single physical drive in the array, leaving remaining member drives unmonitored.
- Manually creating `PhysicalDisk` resources for hidden drives is not supported; the operator marks unbacked resources as `DiskMissing`.

**Recommendation:** Configure storage controllers in **HBA / IT / Pass-Through mode**. This exposes every physical drive directly to the operating system, allowing automatic discovery, independent SMART telemetry, and individual drive failure detection. This is also standard best practice for cloud-native storage solutions.

---

### How do I exclude virtual RAID volumes or specific disks from monitoring?

You can exclude devices from discovery by configuring `discovery.exclude` in `values.yaml`:

```yaml
discovery:
  exclude:
    - "vendor=HP,model=LOGICAL_VOLUME"
    - "name=sdn"
```

Each rule is a comma-separated list of exact key=value matches (`name`, `vendor`, `model`, `serial`, `wwn`, `bus`). All specified fields in a rule must match (AND), and multiple rules act as alternatives (OR).

Excluded devices are:
- Not probed via `smartctl`.
- Not published as `PhysicalDisk` resources.
- Automatically cleaned up: any existing `PhysicalDisk` matching an exclusion rule is deleted during the next inventory scan.

---

### Why do SAS / SCSI disks show fewer telemetry fields than SATA or NVMe drives?

Kubernetes `PhysicalDisk.status.telemetry` only displays fields supported by the drive's hardware protocol:
- **ATA/SATA:** Telemetry includes `reallocatedSectors` (ATA 5) and `pendingSectors` (ATA 197).
- **NVMe:** Telemetry includes `percentageUsed`, `availableSpare`, `criticalWarning`, and `mediaErrors`.
- **SCSI / SAS:** Enterprise SAS drives do not have ATA attributes. Health is evaluated from SCSI Primary Commands (SPC) error logs (`scsi_error_counter_log`) and grown defect lists.

Fields that do not apply to a drive are omitted (`omitempty`) in the resource YAML. A healthy SAS drive typically displays `temperatureCelsius` and `powerOnHours`.

---

### Can I manually create `PhysicalDisk` resources?

No. Node Disk Sentinel continuously reconciles `PhysicalDisk` resources against the node's local udev hardware database. If a resource has no corresponding kernel block device on the host, the reconciler marks it as `DiskMissing`.

---

### How does Node Disk Sentinel identify disks across reboots?

Kernel device names like `/dev/sda` are dynamic and asynchronous; their letter assignments can change across reboots, bus rescans, or when USB drives are attached during boot.

To ensure stability, Node Disk Sentinel implements **predictable device names and paths**:

1. **Predictable Device Paths (`status.info.path`):** The daemon evaluates persistent systemd-udev symlinks under `/dev/disk/` using a strict hierarchy:
   - `/dev/disk/by-id/wwn-*` (World Wide Name; factory burned-in IEEE identifier)
   - `/dev/disk/by-id/nvme-eui.*` (NVMe Extended Unique Identifier)
   - `/dev/disk/by-id/ata-*`, `nvme-*`, `scsi-*` (Serial and model number)
   - `/dev/disk/by-path/*` (Physical PCIe/enclosure slot topology)
   - `/dev/<devname>` (Canonical kernel path as last-resort fallback)

2. **Deterministic Kubernetes Resource Names:** The `PhysicalDisk` custom resource name is derived deterministically from `<node-name>-<hardware-identifier>` (using WWN, serial, or predictable path basename), ensuring Kubernetes object identities, Prometheus metric labels, and alerts remain stable across reboots and controller resets.

---

### Why use a Kubernetes Custom Resource (`PhysicalDisk`) instead of relying solely on Prometheus?

While Prometheus excels at time-series telemetry, historical records, and threshold alerting, relying exclusively on it for infrastructure operations has key limitations in a Kubernetes cluster:

1. **No External Runtime Dependency for In-Cluster Controllers:** Automation controllers can react reliably to disk degradation through standard Kubernetes informers and watches. Requiring them to query an external Prometheus API introduces network dependencies, authentication hurdles, and potential failure points during cluster-level degradations.
2. **Cluster Inventory and Immediate Visibility:** `kubectl get physicaldisks` allows administrators and SREs to instantly inspect physical disk inventory, hardware serials, firmware revisions, and evaluated health states across all nodes without needing access or context-switching to a separate monitoring dashboard.
3. **Declarative Per-Disk Tuning:** The CR's `spec` allows declarative, GitOps-driven configuration (such as `spec.smartmontools.smartctl.extraCmdArgs`) for drives requiring special controller flags or tolerances.
4. **Kubernetes Lifecycle & Events:** State transitions produce native Kubernetes Events on the `PhysicalDisk` object, integrating naturally into existing Kubernetes audit logs and operational tooling.

In summary, **Prometheus is used for telemetry, trends, and monitoring alerts; the `PhysicalDisk` CR is used for cluster inventory, declarative control, and automated controller orchestration.**

---

### Does updating `PhysicalDisk` resources cause high write load or performance issues on etcd?

No. Node Disk Sentinel is designed explicitly to avoid treating `etcd` as a time-series database:

- **No Historical Series in etcd:** The `PhysicalDisk` resource never stores historical data points or time-series logs. Each poll cycle only updates the current evaluated health state, conditions, and a compact operational snapshot (`status.telemetry`), replacing the prior values.
- **Conservative Polling Interval:** Disks are polled every 10 minutes by default (`--poll-interval=10m`). Even in a large cluster with 100 nodes and 6 disks per node (600 disks), this results in approximately 1 write per second across the entire cluster—a fraction of the background load generated by standard node leases and pod heartbeats.
- **Separation of Concerns:** High-frequency metrics and historical downsampling are offloaded entirely to Prometheus via the built-in `:8080/metrics` exporter, which is fully compatible with `smartctl_exporter`.

---

### What industry standards and research is the health assessment based on?

Node Disk Sentinel's evaluation cascade is grounded in formal storage specifications and peer-reviewed empirical failure research:

- **ATA Disks (`libatasmart`):** The ATA evaluation logic implements the prioritized severity cascade from `libatasmart`, the long-standing Linux library utilized across major distributions and disk management utilities.
- **NVMe Drives (NVM Express Base Specification):** Evaluates hardware-level indicators defined in the official NVMe specification, specifically the `Critical Warning` bitmask (temperature, degraded reliability, read-only mode, volatile memory backup failure), `Available Spare` capacity against warning thresholds, and `Percentage Used` (endurance).
- **SCSI / SAS Disks (SCSI Primary Commands / SPC):** Monitors SCSI error counter logs (`scsi_error_counter_log`) and grown defect lists for uncorrected read/write errors as defined by the SPC and SBC standards.
- **Empirical Failure Research (Backblaze Drive Stats):** Large-scale reliability studies on hundreds of thousands of operational drives have demonstrated that bad sectors, specifically raw counts of `Reallocated Sectors` (ATA 5) and `Current Pending Sectors` (ATA 197), as well as NVMe Media Errors, are the strongest statistical leading indicators of impending drive failure, well before manufacturer thresholds are breached or overall self-tests fail.
