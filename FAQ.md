# Frequently Asked Questions (FAQ)

### Can I monitor disks behind a Hardware RAID controller (HPE Smart Array, MegaRAID, Dell PERC)?

Node Disk Sentinel requires a strict **1:1 mapping** between kernel block devices (`/dev/sd*`, `/dev/nvme*n*`) discovered via udev and Kubernetes `PhysicalDisk` resources.

Hardware RAID controllers typically combine multiple physical disks into virtual logical volumes (e.g. `/dev/sda` with model `LOGICAL_VOLUME`), hiding the underlying physical drives from the operating system:
- `smartctl` cannot read native SMART health from virtual RAID volumes directly (failing with errors such as `requires option '-d cciss,N'`).
- Controller passthrough overrides can only query a single physical drive in the array, leaving remaining member drives unmonitored.
- Manually creating `PhysicalDisk` resources for hidden drives is not supported; the operator marks unbacked resources as `DiskMissing`.

**Recommendation:** Configure storage controllers in **HBA / IT / Pass-Through mode**. This exposes every physical drive directly to the operating system, allowing automatic discovery, independent SMART telemetry, and individual drive failure detection. This is also standard best practice for cloud-native storage like Longhorn and Ceph/Rook.

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
