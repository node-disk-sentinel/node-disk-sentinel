# Development & Testing Tooling

This directory provides a lightweight environment for local development, testing, and metrics inspection without requiring a dedicated external Kubernetes cluster.

## Quickstart

```bash
cd hack/

# 1. Create local Kind cluster (mounts disk symlinks and udev metadata)
make cluster-up

# 2. Build local container image, load it into Kind, and deploy charts via Helm
make deploy

# 3. Start local Prometheus & Grafana monitoring stack
make monitoring-up

# 4. Check status of pods, CRDs, and metrics
make status
```

## Service Endpoints & UIs

| Service | URL | Credentials / Notes |
| :--- | :--- | :--- |
| **Grafana** | [http://localhost:3000](http://localhost:3000) | Anonymous Admin access enabled (no login needed). Imports the *"SMARTctl Exporter Dashboard"* (Grafana dashboard `22604`). |
| **Prometheus** | [http://localhost:9090](http://localhost:9090) | Targets overview: [http://localhost:9090/targets](http://localhost:9090/targets). Scrapes every 5s. |
| **Metrics Endpoint** | [http://localhost:8080/metrics](http://localhost:8080/metrics) | Scraped directly from `node-disk-sentinel` on the host, Kind node, or remote Kubernetes node. |

## Scrape Target Configuration

The Prometheus instance in `monitoring/` is configured via `monitoring/prometheus/prometheus.yml`. By default, it scrapes `host.docker.internal:8080`, which is the local Kind node's forwarded metrics port.

To scrape a remote Kubernetes node instead, replace the target with `<node-ip>:8080` and run `make monitoring-up` to recreate the stack with the updated configuration.

## Tear Down

```bash
# Stop monitoring stack
make monitoring-down

# Delete Kind cluster
make cluster-down
```

## Hot-Plugging / Syncing Devices in Kind

In production environments (bare metal or virtual machines running directly as Kubernetes nodes), `node-disk-sentinel` accesses the host's `/dev` via a `hostPath` volume mount. When disks are plugged in or unplugged, the Linux kernel creates or removes device nodes (`/dev/sd*`, `/dev/nvme*n*`) directly in the host's `/dev`, and the daemonset automatically detects them via udev/kernel netlink events.

**In this Kind test setup**, however, the Kubernetes node itself runs inside a Docker container (`nds-dev-control-plane`) with an isolated private `/dev` tmpfs (mounting the host's root `/dev` directly is not possible as it would break container systemd/cgroups). While host udev metadata (`/run/udev/data`) and symlinks (`/dev/disk`) are bind-mounted, **newly connected physical block devices (such as USB drives) do not automatically create device nodes inside the Kind container**.

If you attach or detach a drive while the Kind cluster is already running, run:

```bash
make sync-devices
```

This target scans `/proc/partitions` on the host, creates any missing block device nodes inside the Kind container via `mknod`, and restarts the sentinel pod to trigger an immediate hardware scan.

## Directory Structure

* `Makefile`: Automates Kind cluster creation, building, loading images, deployment, and monitoring.
* `kind-config.yaml`: Kind configuration forwarding port `8080` to the host and mounting host `/dev/disk` and `/run/udev/data`. It deliberately does not mount root `/dev` or `/sys`, because those mounts prevent the Kind node's systemd from starting.
* `monitoring/`: Docker Compose stack containing Prometheus and Grafana configs and the preloaded dashboard.
