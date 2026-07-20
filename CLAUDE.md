# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this project is

`px-smt-multirealm-shim` is a Go HTTPS gateway that enables Portworx **Secure Multi-Tenancy
(SMT)** with multiple FlashArray realms. It sits **between Portworx Enterprise and a Pure Storage
FlashArray REST API**. Portworx is pointed at the shim (via `pure.json`); the shim forwards to
the real FlashArray but **rewrites the specific calls Portworx gets wrong**, so that:

1. **FADA (FlashArray Direct Access) PVCs** whose volumes live in a **realm/pod** can be
   attached to hosts **pre-provisioned at the array level** (non-realm hosts). Portworx can't
   do this natively — it forces host connections into the realm.
2. **Multiple realms** run on **one physical array** (Portworx supports only one realm/array):
   multiple `pure.json` entries, each a distinct FQDN that DNS resolves to the one shim IP; the
   shim makes each FQDN look like a distinct array.

Both are **proven working end to end** (single- and multi-realm FADA attach + I/O).

```
Portworx (px core @ REST 2.2, px-csi @ 2.39)
   │  realm1-fa.demo.pure ┐
   │  realm2-fa.demo.pure ┼─ DNS (<dns-ip>) → one shim ClusterIP (<shim-clusterip>)
   ▼
 px-shim pod ── per-FQDN realm; rewrites host/connection calls; distinct array identity ──► FlashArray <flasharray> (<flasharray-ip>)
```

The shim keys the realm off the **incoming Host FQDN**, and uses an **array-level admin token**
upstream for the calls px's realm-scoped token can't make.

## How the rewrite works (the core of the shim)

px creates **realm hosts** `<realm>::<node>-<nodeUID>` and tries to give them the node's
initiators — which 400s because those initiators already belong to the pre-provisioned
**array-level host** `ocp4-1-worker<N>`. Initiators cover all three transports: **iSCSI (IQNs),
FC (WWNs), NVMe-TCP (NQNs)** — each configured per host as a comma-separated list (`iqns`/`wwns`/
`nqns`). The shim (keyed by the stable `<node>` prefix → array-level host, from a static config)
does:

- `PATCH /hosts …add_iqns`/`…add_wwns`/`…add_nqns` on a realm host → **synthetic 200** (the
  initiators already live on the array host).
- `GET /hosts` → **inject** the mapped initiators into realm-host items (so px sees its host owns
  them and skips the failing patch).
- `GET /hosts?names=<realm>::<arrayHost>` (the double-prefixed name px derives during FADA
  attach) → **synthesize** the host (it doesn't exist on the array).
- `GET/POST/DELETE /connections` referencing a realm host → **rewrite host_names → array host**,
  issued with the **array-level token**, and **map the array host name back** to the realm host
  in the response (so px only ever sees its own name; otherwise it re-queries a double-prefixed
  name that exists nowhere). Duplicate POST connection = treated as 200.
- **Grant the array host access into the realm before connecting.** A `POST /connections` that
  attaches a realm/pod volume to the array-level host is **refused by Purity unless that host has
  been granted access into the realm** — i.e. it fails exactly as if the shim weren't there. So on
  the connect the shim first issues `POST /api/2.40/resource-accesses/batch`
  (`{resource:{host}, scope:{realm}}`, array-admin token), cached per `(arrayHost, realm)` and
  treating "already exists" (400) as success. This is the grant the `flasharray-rest-ops` skill
  documents, now made automatic. `ensureRealmAccess` in `rewrite.go`. **REST version matters:**
  `resource-accesses` first appears at **2.40** (2.36/2.38 → 404) — verified on the lab array
  (6.10.6, REST max 2.54); prod 6.12 (2.56) is newer. Coverage: `fada_sequence_test.go` (offline
  mock enforcing the grant rule) and `fada_integration_test.go` (`-tags integration`, drives the
  shim against a **real** array via `IT_*` env; a non-mutating grant check + a self-cleaning
  end-to-end connect using `eg-ittest`-prefixed objects). Build the test binary on the dev host
  (`go test -tags integration -c -o fada.itest ./internal/proxy`), `scp` to the jump host, run
  with `IT_ARRAY_URL`/`IT_ARRAY_TOKEN`/`IT_REALM`/`IT_POD`/`IT_NODE`/`IT_ARRAY_HOST`/`IT_IQNS`.
- **`GET /connections?volume_names=…` (no host filter) — map the array host name back too.** px
  lists a volume's connections without naming the host; that response leaks the array host name,
  and px (realm-scoped) prepends its realm → a double-prefixed `<realm>::<arrayHost>` it then
  DELETEs and re-POSTs under its real realm host — a ~30s **connection-churn loop** that
  reassigns the LUN every cycle until the multipath device goes read-only and **FADA writes are
  lost**. Fix: the shim **learns `arrayHost → <realm>::<node>-<uid>`** from px's own host-filtered
  requests (`connection()` + GET /hosts listings) and rewrites the array host name back in **every**
  `/connections` response (`ModifyResponse` → `mapConnHostsBack`), so px always sees its own name
  and connects **once**. This was the primary FADA data-loss cause; see the LUN-recycle section.
- **Per-FQDN array identity**: rewrite `GET /arrays` (and everywhere the array name/id appears)
  so each realm FQDN looks like a distinct array (`<flasharray>-<label>` + a per-FQDN synthetic id);
  map synth→real on requests. Without this, px de-duplicates the realms and mis-routes volumes.
- Everything else: transparent pass-through. The rewrite matches `/hosts`/`/connections`/
  `/arrays` by path **suffix**, so it is REST-version-agnostic (covers px core 2.2 and px-csi
  2.39).

Code: `internal/proxy/` — `rewrite.go` (Rewriter, keyed off `config.Hosts`), `array.go`
(array-level session), `arrayident.go` (per-FQDN array identity), wired in `proxy.go`
(request `Intercept`/`RewriteRequest` + `ModifyResponse`). Toggle with `SHIM_REWRITE`.

## ⚠️ The FADA LUN-recycle data-loss bug — and the `fada-cleaner` (defense-in-depth)

> **Primary FADA data-loss cause was the connection-churn loop above (fixed in the shim,
> `mapConnHostsBack`).** Once px connects **once** (no churn), its own detach cleanup works —
> in the end-to-end failover test the cleaner flushed **0** maps and 0 stale maps remained. The
> `fada-cleaner` below stays as **defense-in-depth** for detach/recycle edge cases (a detach px
> doesn't fully clean, or a genuine LUN-recycle race).

Connecting a realm/pod volume to an array-level host (the shim's core rewrite) also exposed
**silent data corruption on detach/reattach** under LUN churn. Root cause is the classic
Cinder-style **LUN recycling**: the FlashArray reuses LUN numbers, so a detach/reattach is *not* guaranteed
the same LUN. Portworx's CSI path does **not** do a full host-side disconnect on detach — it
leaves a stale multipath map + SCSI `/dev/sd*` devices behind (worse in the workaround, where
px-csi reconciles the *realm-host* LUN while the real device came in via the *array-level host*
at a different LUN, so it flushes the wrong one). When the array re-hands that freed LUN to a
**different** volume, the stale device handle silently serves the new volume's blocks → EXT4
superblock write failures, read-only fs, `total_physical=0`.

**Proven cold** (raw-device repro, scoped to `eg-recycle-*`): the same `/dev/sdX` that backed
volA@LUN-N came back reporting volB's WWID after the array recycled LUN-N; reads through the old
map no longer returned volA's data. **Standard FADA (realm host, no shim rewrite) cleans up
correctly** — the leak is specific to the array-level-host workaround (and any tight
detach/reattach that beats px's cleanup).

**Fix — `cmd/fada-cleaner/` (per-node privileged DaemonSet), the disconnect px-csi omits:** for a
`PURE,FlashArray` multipath map that is **unused** (device-mapper open count 0) **and stale**, it
does a **full disconnect** — `multipath -f <wwid>` then `echo 1 > /sys/block/sd*/device/delete`
for every path — so the LUN is fully logged out and can't be silently reused. All host ops go
through `nsenter -t 1` (needs `hostPID` + privileged).

Trigger + safety (v2/v3):
- **Detach signal:** a stdlib in-cluster **`VolumeAttachment` watch** (RBAC: get/list/watch,
  filtered to this node via `NODE_NAME`) fires the moment a volume genuinely leaves the node —
  which a same-node *pod restart* does **not** do (the attachment persists). A ≤8s **poll** is the
  backstop.
- **Path-health gate (the key safety):** open-count 0 alone is not enough — a briefly-unmounted
  but still-attached volume (pod restart) is also open 0. A map is flushed only if it is also
  *stale*: a path whose `scsi_id` now resolves to a **different** WWID (LUN recycled to another
  volume — the corruption itself), or **no path still healthy** (`scsi_id == wwid && state
  running`); a removed LUN fails the INQUIRY (empty id) and counts as unhealthy. Because this is
  only consulted on idle (open 0) maps, a healthy path reliably answers, so pod restarts of *any*
  duration are safe. On a detach event the flush is immediate; on the poll path a 2-scan grace
  adds hysteresis. `hostCmd` has a 15s timeout so a yanked/blocked device can't stall the loop.
- **In-use volumes** (mounted / held by PX-StoreV2) have open count >0 and are never touched.
- **Transport-agnostic, incl. FC / `user_friendly_names` / aliases.** `multipath -ll` emits two
  header forms: plain `<wwid> dm-N vendor` (friendly-names off) and `<alias> (<wwid>) dm-N vendor`
  (FC hosts and any host with aliases). The parser handles both and tracks the **map name**
  (alias or WWID) separately from the **WWID**: open-count/flush key off the name (the dm map is
  named by the alias, not the WWID), `scsi_id` staleness keys off the WWID. Earlier the parser
  only matched the plain form, so FC/aliased maps were silently skipped and never cleaned.
- **Observability:** each scan emits a `scan trigger=… pure=… idle=… stale=… flushed=…` summary
  when the result changes or something is flushed, and at least once per `FADA_HEARTBEAT_SECONDS`
  (default 300) even when idle — so `oc logs` always shows recurring liveness instead of going
  silent after the two startup lines.

Validated end-to-end: flushed only dead orphans (cross-checked against the array — no live
volume/connection), left `open=1` cloud drives alone, px stayed 3/3; guarded repro showed the
recycle collision **prevented** (fresh map, clean data); a healthy open-0 map **survives** past
grace while a genuinely detached one **flushes** (`reason=no-healthy-path`/`scsi-id-mismatch`).

Env: `FADA_POLL_SECONDS`, `FADA_HEARTBEAT_SECONDS` (default 300), `FADA_GRACE_POLLS`,
`FADA_VENDOR` (default `PURE`), `FADA_DRY_RUN`
(deploy with `true` first to verify classification, then flip). Build: `Dockerfile.cleaner`
(debian-slim + util-linux for `nsenter`) → `docker push` via loopback `127.0.0.1:5000` (dev-host
docker only trusts `127.0.0.0/8` insecure) → sideload `podman pull` → `deploy/px-fada-cleaner.yaml`
(own `px-fada-cleaner` ns, PSA `privileged`, SA bound to the `privileged` SCC + a ClusterRole for
the watch). Use a fresh image tag per iteration (`imagePullPolicy: IfNotPresent`).

## ⚠️ ARRAY SAFETY RULE — only touch what we created

The FlashArray is **shared** with other tenants/realms (`<tenant-a>`, `<tenant-b>`, `ocp4-1`,
`ocp4-1-a`, `<other-array>-*`, …). **Never delete or modify an array object we did not create.** Scope
every create to our own names; keep the inventory below current; delete **only** objects
matching our exact prefixes. (A past mistake deleted `ocp4-1-a::worker1` via an over-broad
`::worker` match — match the full `ocp4-1-realm{1,2}::` prefix.) See the `flasharray-rest-ops`
skill for REST recipes and deletion order.

**Objects we created on `<flasharray>` (`<flasharray-ip>`), ours to manage:**
- Array-level hosts `ocp4-1-worker0/1/2` (node IQNs `…:<node0-iqn>`, `…:<node1-iqn>`,
  `…:<node2-iqn>`).
- Realms `ocp4-1-realm1`, `ocp4-1-realm2`; pods `ocp4-1-realm1::pxe-1`, `ocp4-1-realm2::pxe-2`.
- Realm admins `eg-ocp4-1-realm{1,2}`, policies `ocp4-1-realm{1,2}-pol`, resource-access grants
  (3 hosts × 2 realms).
- **Safe-to-delete leftovers px creates:** volumes under `ocp4-1-realm1::pxe-1::` /
  `ocp4-1-realm2::pxe-2::` (`pxclouddrive-*`, `px_*pvc-*`); dynamic realm hosts
  `ocp4-1-realm{1,2}::worker*-<uid>`. **Cleanup deletes ONLY these** — keep hosts/realms/pods/
  admins/policies/grants unless fully deprovisioning; never touch anything outside our realms.

## Environment & access

No credentials in this repo. All access via the jump host.

| Host | Purpose |
|------|---------|
| `<jump-host-ip>` | **Jump host** (SSH `<ssh-user>`). Gateway to `oc` and the array. `oc`/`kubectl` in `~/.local/bin`; `export KUBECONFIG=~/.kube/ocp4-1.yaml`. |
| `<dev-host-ip>` | **Dev/build host** (`<ssh-user>@…`, `<dev-host>`). Docker 27.3 (no Go/podman → multi-stage Docker build). |
| OpenShift 4.21.15 | Nodes `master0-2`, `worker0-2`; px runs on workers. Operator: OLM `portworx-certified` / `portworx-operator.v26.2.1` in `openshift-operators`. |
| FlashArray `<flasharray>` | `<flasharray-ip>`, Purity 6.10.6, REST max 2.54. Everpure/FADA. iSCSI (`ens34.31,ens35.32`). |

**DNS** (`<dns-ip>`, user-managed, serves `demo.pure` and is the nodes'/px hostNetwork
resolver): `realm1-fa.demo.pure` and `realm2-fa.demo.pure` → shim ClusterIP `<shim-clusterip>`.

**`px-pure-secret`** (ns `portworx`, key `pure.json`) points px at the shim. Bring-up uses a
**single realm** (`realm1`); add realm2 after the cluster is healthy (then rolling-restart px so
it re-reads the backend list):
```json
{ "FlashArrays": [
  {"MgmtEndPoint":"realm1-fa.demo.pure","APIToken":"<realm1 token>","Realm":"ocp4-1-realm1"},
  {"MgmtEndPoint":"realm2-fa.demo.pure","APIToken":"<realm2 token>","Realm":"ocp4-1-realm2"}
] }
```

## Deploying the shim

- Manifest: `deploy/px-shim.yaml` (ns `px-shim`, Deployment + ClusterIP Service on 443→9443).
- Config secret `px-shim-config`: `config.json` (host map, below) + `array-token` (array admin).
  Per host, list the initiators it owns per transport in `iqns` (iSCSI), `wwns` (FC), and/or
  `nqns` (NVMe-TCP); set only the transports the node uses. **Every list uses the same syntax:
  comma-separated entries** (the `:` inside an IQN/NQN is part of the identifier, not a separator).
  Legacy singular `iqn` is still accepted (folded into `iqns`).
  ```json
  { "hosts": [
    {"node":"worker0","arrayHost":"ocp4-1-worker0","iqns":"iqn.1994-05.com.redhat:<iqn-a>,iqn.1994-05.com.redhat:<iqn-b>"},
    {"node":"worker1","arrayHost":"ocp4-1-worker1","wwns":"<wwn-a>,<wwn-b>"},
    {"node":"worker2","arrayHost":"ocp4-1-worker2","nqns":"<nqn-a>,<nqn-b>"}
  ] }
  ```
- Build/iterate: build on the dev host → push to a local `registry:2` (`<dev-host-ip>:5000`) →
  **sideload** onto nodes (`podman pull --tls-verify=false …`) → `oc set image` with a **fresh
  tag** (`imagePullPolicy: IfNotPresent`). See the `portworx-storagecluster-lifecycle` skill §5.
- Env: `SHIM_UPSTREAM_URL=https://<flasharray-ip>`, `SHIM_UPSTREAM_INSECURE=true`,
  `SHIM_CERT_SANS=realm1-fa.demo.pure,realm2-fa.demo.pure`, `SHIM_CONFIG_FILE`, `SHIM_ARRAY_TOKEN`
  (from the secret), `SHIM_REWRITE=true`, `SHIM_ARRAY_IDENTITY` (default `true`).
- **`SHIM_ARRAY_IDENTITY=false` for single-realm deployments.** The per-FQDN synthetic array
  identity (below) exists only to stop px de-duplicating **multiple realms** on one array. With a
  single realm (+ a non-realm `common` endpoint) it has no de-dup to prevent and actively breaks
  FADA: the DirectAccess attacher resolves a volume's backend to the array's **real** id (from the
  device), so that id must be in px's endpoint map — but the synth id replaces it, and the attach
  fails with `DirectAccessIdentifier … backend ID <real-uuid> … not present in NFS endpoints map`
  (the volume connects and the multipath device comes up; only the serial/backend lookup fails).
  The host/connection/grant rewrites are independent of this and stay on.

## StorageCluster (Portworx) deploy

Single-realm `pure.json`, `cloudStorage.deviceSpecs: size=150,pod=pxe-1` +
`systemMetadataDeviceSpec: size=64,pod=pxe-1`, `secretsProvider: k8s` (NOT vault-transit — it
blocks bring-up here), `deleteStrategy: {type: UninstallAndDelete}`. **Use a fresh cluster name
for each clean install.** FADA StorageClass:
```yaml
provisioner: pxd.portworx.com
parameters: { backend: "pure_block", pure_fa_pod_name: "pxe-1" }   # realm from pure.json; pod name REQUIRED in a realm
```

For uninstalling / recovering / reset gotchas (storageless deadlock, `MaxProvisionAttemptsReached`,
degraded MCP from node surgery), see the **`portworx-storagecluster-lifecycle`** skill.

## Stress testing (`cmd/pxstress`)

A configurable, dependency-free Go harness that stresses the FADA-PVC path. Each **pool** is a
StatefulSet of single-node etcd instances (one FADA PVC per replica) in its own namespace; pools
run concurrently and cycle `min → mid → max → mid`, deleting the now-unused PVCs, and are
periodically deleted + recreated wholesale. Every step verifies **data integrity** (a per-instance
etcd key must survive) and each cycle does a **kill+reattach durability check** (delete a pod →
force FADA detach/reattach → confirm the key survived). It shells out to `oc` with the caller's
kubeconfig, so it builds to a static binary and runs from the jump host:

```
pxstress -pools=3 -min=1 -mid=3 -max=5 -duration=1h -recreate-every=2 -cmd-timeout=5m -stop-on-error
```

Build like the shim (dev-host `docker run golang:1.23 go build`), `scp` the binary to the jump
host, run with `KUBECONFIG` set. `-cmd-timeout` must be generous (FADA detach under heavy
concurrent churn can take minutes). **Result on this cluster:** a 1-hour, 3-pool run completed
with **0 failures / 0 data loss** — 22 scale cycles, 24 kill+reattach durability passes, 12 pool
recreates — validating the connection-churn fix + cleaner under sustained load.

## Glossary

- **`pure.json`** — Portworx's FlashArray config (secret `px-pure-secret`); repointing its
  `MgmtEndPoint` at the shim is how px is diverted. `Realm` scopes px to one realm.
- **FADA** — FlashArray Direct Access: PVCs backed by FlashArray volumes attached to the host
  (here `backend: pure_block`), vs Portworx-pooled StoreV2 cloud drives.
- **Realm / Pod (Purity)** — tenancy constructs; a realm contains pods. The bug is crossing that
  boundary: connecting a realm/pod volume to an array-level (non-realm) host.
