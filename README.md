# bloc-manager

Orchestration backend for the [Blocstor](https://github.com/blocstor) DRBD storage stack.
Manages DRBD volume lifecycle, coordinates [bloc-agent](https://github.com/blocstor/bloc-agent) daemons on storage nodes.
Consumed by [bloc-csi](https://github.com/blocstor/bloc-csi).

## Overview

bloc-manager runs as a Pacemaker-managed active/passive service (not a Kubernetes pod). It:

- Maintains a SQLite database of DRBD volumes (volume ID, LV name, minor number, size, attached node)
- Exposes a REST API consumed by bloc-csi
- Coordinates bloc-agent instances to create, attach, resize, and destroy DRBD volumes

Minor numbers start at 1000 and increment per volume.

## Configuration

### agents.yaml

```yaml
agents:
  storage-node-1: http://192.168.0.57:8080
  storage-node-2: http://192.168.0.58:8080
```

Each key is the DRBD node name — it must match the `on <name>` identifier in your `.res` files.
bloc-manager uses this name when generating DRBD resource definitions; it cannot be discovered from the agent itself.
The value is the bloc-agent HTTP API base URL; use an IP or hostname as suits your environment.

### CLI flags

| Flag          | Default                          | Description                    |
|---------------|----------------------------------|--------------------------------|
| `--listen`    | `:9090`                          | HTTP listen address            |
| `--db`        | `/var/lib/bloc-manager/state.db` | Path to SQLite database        |
| `--agents`    | `agents.yaml`                    | Path to agents config file     |
| `--log-level` | `info`                           | Log level (debug/info/warn/error) |

## API

### Volumes

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/volumes` | Create a volume |
| `GET` | `/volumes` | List all volumes |
| `GET` | `/volumes/:id` | Get a volume |
| `DELETE` | `/volumes/:id` | Destroy a volume |
| `POST` | `/volumes/:id/publish` | Promote DRBD to Primary on a node |
| `DELETE` | `/volumes/:id/publish` | Demote DRBD to Secondary on all nodes |
| `POST` | `/volumes/:id/resize` | Resize a volume |
| `GET` | `/healthz` | Health check |

### Create volume

```
POST /volumes
Content-Type: application/json

{
  "name": "vol-pvc-abc123",
  "size_mb": 10240,
  "nodes": ["cs1", "cs2"]
}
```

Response `201 Created`:

```json
{
  "id": "a3f8c1d2e5b7f094",
  "name": "vol-pvc-abc123",
  "minor": 1000,
  "size_mb": 10240,
  "status": "available",
  "nodes": ["cs1", "cs2"]
}
```

### Publish volume

```
POST /volumes/:id/publish
Content-Type: application/json

{"node": "cs1"}
```

Response `200 OK`:

```json
{
  "node": "cs1",
  "device": "/dev/drbd1000"
}
```

### Resize volume

```
POST /volumes/:id/resize
Content-Type: application/json

{"new_size_mb": 20480}
```

Response `200 OK`:

```json
{"id": "a3f8c1d2e5b7f094", "size_mb": 20480}
```

### Errors

All error responses use the format:

```json
{"error": "human-readable message"}
```

## High Availability

bloc-manager runs as a Pacemaker-managed active/passive service. Its SQLite state is stored on a small dedicated DRBD volume so the standby node can take over with full state intact — no external database required.

### DRBD volume for state

Create a small DRBD resource (1 GB is more than enough) on the two manager nodes:

```ini
# /etc/drbd.d/bloc-manager-state.res
resource bloc-manager-state {
  protocol C;          # synchronous — zero data loss on failover
  net { allow-two-primaries no; }

  on mgr1 {
    device    /dev/drbd100;
    disk      /dev/sdb1;
    address   192.168.0.10:7900;
    meta-disk internal;
  }
  on mgr2 {
    device    /dev/drbd100;
    disk      /dev/sdb1;
    address   192.168.0.11:7900;
    meta-disk internal;
  }
}
```

Initialize once:

```bash
drbdadm create-md bloc-manager-state
drbdadm up bloc-manager-state
# on the first primary only:
drbdadm primary --force bloc-manager-state
mkfs.ext4 /dev/drbd100
```

### Installation (all manager nodes)

Install the binary and config on every node that may run bloc-manager:

```bash
install -m 0755 bloc-manager /usr/local/bin/bloc-manager
install -m 0755 -d /etc/bloc-manager
install -m 0644 agents.yaml /etc/bloc-manager/agents.yaml
```

Install the systemd unit (included in this repo as `systemd/bloc-manager.service`):

```bash
install -m 0644 systemd/bloc-manager.service /etc/systemd/system/bloc-manager.service
systemctl daemon-reload
```

> **`Restart=no`** — Pacemaker manages restarts and fencing. systemd must not restart the process after Pacemaker kills it during failover.

Do not `systemctl enable` bloc-manager — Pacemaker starts and stops it exclusively.

### Pacemaker resource group

Pacemaker starts resources in order and stops them in reverse. The entire group fails over atomically.

```bash
pcs resource create drbd-mgr-state ocf:linbit:drbd \
    drbd_resource=bloc-manager-state \
    op monitor interval=20s

pcs resource promotable drbd-mgr-state \
    promoted-max=1 promoted-node-max=1 clone-max=2 clone-node-max=1

pcs resource create fs-mgr-state Filesystem \
    device=/dev/drbd100 directory=/var/lib/bloc-manager fstype=ext4

pcs resource create vip-mgr IPaddr2 \
    ip=192.168.0.20 cidr_netmask=24

pcs resource create bloc-manager systemd:bloc-manager

pcs resource group add bloc-manager-group \
    fs-mgr-state vip-mgr bloc-manager

pcs constraint order promote drbd-mgr-state-clone \
    then start bloc-manager-group

pcs constraint colocation add bloc-manager-group \
    with promoted drbd-mgr-state-clone score=INFINITY
```

On failover, Pacemaker executes the resource group in order on the surviving node:

1. **Fence** the failed node via STONITH — prevents split-brain before touching shared state
2. **Promote DRBD** — `/dev/drbd100` becomes Primary on the survivor
3. **Mount** `/dev/drbd100` → `/var/lib/bloc-manager` — SQLite file is now present
4. **Bring up the VIP** on the survivor's NIC
5. **Start bloc-manager** — Pacemaker's `systemd:bloc-manager` resource agent runs `systemctl start bloc-manager` on the survivor; bloc-manager opens the SQLite file at its configured `--db` path and begins serving

bloc-manager has no awareness of the failover. It simply starts, finds its database at the expected path, and serves the VIP. bloc-csi reconnects to the same VIP address — no reconfiguration needed.

> **Note:** bloc-manager must not hold in-memory state that isn't persisted to SQLite (e.g. the minor number counter, in-flight operation state). Anything not in the database at the moment of failure is lost on failover.

### STONITH

Fencing is mandatory. Without it Pacemaker will not promote DRBD after a partition (split-brain protection). Configure an IPMI/iDRAC fence agent for each manager node:

```bash
pcs stonith create fence-mgr1 fence_ipmilan \
    ipaddr=192.168.1.10 login=admin passwd=secret pcmk_host_list=mgr1
pcs stonith create fence-mgr2 fence_ipmilan \
    ipaddr=192.168.1.11 login=admin passwd=secret pcmk_host_list=mgr2
```

## Building

CGO is required for the SQLite driver.

```bash
CGO_ENABLED=1 go build -o bloc-manager ./cmd/manager
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
