# px-smt-multirealm-shim

A Go HTTPS gateway that sits **between Portworx Enterprise and a Pure Storage FlashArray REST
API** to enable **Portworx Secure Multi-Tenancy (SMT)** on a shared FlashArray.

Portworx is pointed at the shim (via its `pure.json` backend config); the shim forwards every
call to the real FlashArray but **rewrites the specific calls Portworx gets wrong**, so that two
things become possible that Portworx cannot do natively:

1. **FADA (FlashArray Direct Access) PVCs** whose volumes live in a **realm/pod** can be attached
   to hosts **pre-provisioned at the array level** (non-realm hosts). Portworx forces host
   connections into the realm; the shim redirects them to the array-level host.
2. **Multiple realms on one physical array.** Portworx supports only one realm per array. The
   shim makes each realm's FQDN look like a **distinct array**, so several `pure.json` entries —
   all resolving via DNS to the one shim IP — drive independent realms on the same hardware.

Both are proven working end to end (single- and multi-realm FADA attach + sustained I/O).

> **Note:** the Kubernetes resource names (namespace, Service), the container image, and the
> config secret use the short identifier `px-shim`, kept stable so the DNS→ClusterIP mapping and
> existing deployments keep working.

## Architecture

```
Portworx (px core @ REST 2.2, px-csi @ 2.39)
   │  realm1-fa.demo.pure ┐
   │  realm2-fa.demo.pure ┼─ DNS (<dns-ip>) → one shim ClusterIP (<shim-clusterip>)
   ▼
 shim pod ── per-FQDN realm; rewrites host/connection calls; distinct array identity ──► FlashArray <flasharray>
```

The shim keys the realm off the **incoming Host FQDN** and uses an **array-level admin token**
upstream for the calls Portworx's realm-scoped token cannot make. All matching is by URL path
**suffix**, so the rewrites are REST-version-agnostic.

### What the rewrite does

Portworx creates **realm hosts** and tries to give them a node's initiators — which fails because
those initiators already belong to the pre-provisioned **array-level host**. Initiators span every
supported transport: **iSCSI (IQNs), Fibre Channel (WWNs), and NVMe-TCP (NQNs)**. Keyed by a static
node→array-host map, the shim:

- Synthesizes a `200` for the `PATCH /hosts …add_iqns` / `…add_wwns` / `…add_nqns` that would
  otherwise `400`.
- Injects the mapped initiators into realm-host items on `GET /hosts` so Portworx skips the failing patch.
- Synthesizes the double-prefixed host Portworx derives during FADA attach.
- Rewrites `/connections` host names to the array-level host (issued with the array token) and
  **maps the array host name back** in every response — including the un-filtered
  `GET /connections?volume_names=…` — so Portworx only ever sees its own realm host and connects
  **once**. This eliminates a ~30 s connection-churn loop that was the primary FADA data-loss cause.
- Rewrites `GET /arrays` (and every place the array name/id appears) so each realm FQDN presents a
  **distinct array identity**, preventing Portworx from de-duplicating the realms.

Everything else is transparent pass-through. Toggle the rewrites with `SHIM_REWRITE`.

Code: [`internal/proxy/`](internal/proxy/) — `rewrite.go` (the Rewriter), `array.go` (array-level
session), `arrayident.go` (per-FQDN array identity), wired in `proxy.go`.

## Repository layout

| Path | What it is |
|------|------------|
| [`cmd/px-smt-multirealm-shim/`](cmd/px-smt-multirealm-shim/) | The gateway binary (main entrypoint). |
| [`cmd/fada-cleaner/`](cmd/fada-cleaner/) | Per-node privileged DaemonSet that performs the host-side disconnect px-csi omits on detach — defense-in-depth against the FlashArray LUN-recycle data-loss race. |
| [`cmd/pxstress/`](cmd/pxstress/) | Dependency-free FADA-PVC stress harness (concurrent pools of etcd StatefulSets, data-integrity + kill/reattach durability checks). |
| [`internal/proxy/`](internal/proxy/) | The reverse proxy and rewrite logic. |
| [`internal/config/`](internal/config/) | Env-driven runtime config + self-signed server cert generation. |
| [`deploy/`](deploy/) | Kubernetes manifests (`px-shim.yaml`, `px-fada-cleaner.yaml`). |
| [`scripts/`](scripts/) | `build-and-push.sh` — tar → dev host → multi-stage Docker build → registry. |

## The FADA LUN-recycle data-loss bug (and the cleaner)

Connecting a realm/pod volume to an array-level host also exposed silent data corruption on
detach/reattach under LUN churn: the FlashArray reuses LUN numbers, and Portworx's CSI path does
not fully disconnect on detach, leaving a stale multipath map + SCSI devices behind. When the
array re-hands a freed LUN to a different volume, the stale device handle serves the new volume's
blocks → read-only filesystem, corrupted superblocks.

The primary cause (the connection-churn loop) is fixed in the shim. As **defense-in-depth**,
[`cmd/fada-cleaner/`](cmd/fada-cleaner/) runs a privileged per-node DaemonSet that, for a
`PURE,FlashArray` multipath map that is both **unused** (device-mapper open count 0) **and stale**
(a path whose `scsi_id` now resolves to a different WWID, or no path still healthy), performs the
full disconnect — `multipath -f <wwid>` then `echo 1 > /sys/block/sd*/device/delete` — so the LUN
is fully logged out and cannot be silently reused. In-use volumes (open count > 0) are never
touched. A `VolumeAttachment` watch is the detach signal; a poll is the backstop.

## Building

The build is a multi-stage Docker build (standard library only — no module proxy needed):

```sh
# from a host with Docker:
docker build -t <registry>/px-shim:<tag> .

# or use the relay script (workstation → jump host → dev host → registry):
scripts/build-and-push.sh
```

The `fada-cleaner` image uses `Dockerfile.cleaner` (debian-slim + util-linux for `nsenter`).

## Deploying

Manifests live in [`deploy/`](deploy/).

1. **Shim** — [`deploy/px-shim.yaml`](deploy/px-shim.yaml): namespace `px-shim`, a Deployment, and
   a ClusterIP Service (443 → 9443). It reads a `px-shim-config` secret containing:
   - `config.json` — the static node → array-host → initiators map. Each host lists the initiator
     identifiers it owns per transport as **comma-separated** `iqns` (iSCSI), `wwns` (FC), and/or
     `nqns` (NVMe-TCP); set only the transports a node actually uses:
     ```json
     { "hosts": [
       {"node":"worker0","arrayHost":"ocp4-1-worker0","iqns":"iqn.1994-05.com.redhat:<node-iqn>"},
       {"node":"worker1","arrayHost":"ocp4-1-worker1","wwns":"<wwn-a>,<wwn-b>"},
       {"node":"worker2","arrayHost":"ocp4-1-worker2","nqns":"<node-nqn>"}
     ] }
     ```
     The legacy singular `"iqn"` key is still accepted (folded into `iqns`) for older secrets.
   - `array-token` — the FlashArray **array-level admin** API token (for the rewritten connection
     calls the realm-scoped token cannot make).
2. **DNS** — point each realm FQDN (`realm1-fa.demo.pure`, `realm2-fa.demo.pure`, …) at the shim's
   ClusterIP.
3. **Portworx** — set `px-pure-secret`'s `pure.json` `MgmtEndPoint` for each realm to its FQDN:
   ```json
   { "FlashArrays": [
     {"MgmtEndPoint":"realm1-fa.demo.pure","APIToken":"<realm1 token>","Realm":"ocp4-1-realm1"},
     {"MgmtEndPoint":"realm2-fa.demo.pure","APIToken":"<realm2 token>","Realm":"ocp4-1-realm2"}
   ] }
   ```
4. **Cleaner** (optional, recommended) — [`deploy/px-fada-cleaner.yaml`](deploy/px-fada-cleaner.yaml).

### Configuration (environment variables)

| Variable | Purpose |
|----------|---------|
| `SHIM_UPSTREAM_URL` | Real FlashArray URL, e.g. `https://<flasharray-ip>`. |
| `SHIM_UPSTREAM_INSECURE` | Skip verification of the FlashArray cert (`true` in dev). |
| `SHIM_CERT_SANS` | Comma-separated SANs for the shim's self-signed server cert (the realm FQDNs). |
| `SHIM_CONFIG_FILE` | Path to `config.json` (the host map). |
| `SHIM_ARRAY_TOKEN` | Array-level admin token (from the secret). |
| `SHIM_REWRITE` | Enable the rewrites (`true`); `false` = transparent pass-through. |

The `fada-cleaner` uses `FADA_POLL_SECONDS`, `FADA_GRACE_POLLS`, `FADA_VENDOR` (default `PURE`),
and `FADA_DRY_RUN`.

## ⚠️ Array safety

The FlashArray is typically **shared** with other tenants/realms. Never delete or modify an array
object this project did not create. Scope every create to your own names and delete only objects
matching your exact prefixes.

## Status

Proven working end to end for single- and multi-realm FADA attach + I/O. A 1-hour, 3-pool
`pxstress` run completed with **0 failures / 0 data loss** across 22 scale cycles, 24 kill+reattach
durability checks, and 12 pool recreates.
