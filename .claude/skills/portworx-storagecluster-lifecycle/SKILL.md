---
name: portworx-storagecluster-lifecycle
description: How to correctly uninstall a Portworx (PX-StoreV2/FlashArray) StorageCluster on OpenShift, recover from a botched delete via manual per-node wipe, and reset cleanly for a fresh install. Use when deleting/reinstalling Portworx, when px is stuck (storageless deadlock, MaxProvisionAttemptsReached), or when the worker MachineConfigPool went Degraded after touching node files.
---

# Portworx StorageCluster lifecycle (OpenShift + FlashArray/PX-StoreV2)

Node access throughout: `oc debug node/<n> --quiet -- chroot /host bash -c '…'`.

## 1. Delete a StorageCluster the RIGHT way

Set a delete strategy **before** deleting, or px keeps running on each node via its host
`systemd` unit even after the CR and pods are gone.

- `spec.deleteStrategy.type`: `Uninstall` (remove px, keep data) | `UninstallAndWipe` (also
  wipe devices/metadata) | `UninstallAndDelete` (also delete the drives from each driveset).
- Add `spec.deleteStrategy.ignoreVolumes: true` if the operator refuses with "volume(s) are
  still in use" (a false positive when px never fully formed).

```bash
oc patch storagecluster <name> -n <ns> --type merge \
  -p '{"spec":{"deleteStrategy":{"type":"UninstallAndDelete","ignoreVolumes":true}}}'
oc delete storagecluster <name> -n <ns> --wait=false
```

The operator runs a **`px-node-wiper` DaemonSet** (`removeData=true`) that stops px, wipes
drives, and clears the px metadata ConfigMaps. Watch: `oc get pods -n <ns> | grep wiper` and
the operator log (`Node Wiper Status: Completed [n] …`). It can take several minutes; the CR
deletes when all nodes report Completed. The wiper leaves `/etc/pwx` and `/opt/pwx` on nodes —
remove them manually (step 3) for a truly fresh reinstall.

## 2. Recover from a WRONG delete (CR deleted with no strategy)

Symptom: `oc get storagecluster` empty, but `systemctl is-active portworx` is `active` on nodes
and `pxctl status` still works. You must **manually uninstall px on every node**. The
PX-StoreV2 storage stack is layered and must be torn down **top-down** or devices are "in use":

```
FlashArray iSCSI → multipath (/dev/mapper/3624a937…) → md raid0 (md127) → LVM VG pwx0
   (thin pool pxpool = _tmeta/_tdata/-tpool; thin LVs pxMetaFS, pxpool, + per-volume thins)
```

Per node:
1. `systemctl stop portworx.service portworx-output.service portworx.socket` (+ `disable`).
2. Detach drives: `iscsiadm -m node -u` → `iscsiadm -m node -o delete` → `multipath -F`.
3. Remove LVM top-down: remove thin volumes first (any `pwx0-*` that is NOT `-tpool`/`_tdata`/
   `_tmeta`), then `pwx0-pxpool-tpool`, then `pwx0-pxpool_tdata`/`_tmeta`
   (`dmsetup remove --retry`). The per-volume thin set varies per node — **enumerate, don't
   hardcode**.
4. `mdadm --stop /dev/md127`, then `dmsetup remove --force` the leftover `3624a937…` multipath map.
5. Remove config: `/etc/systemd/system/portworx*.{service,socket}` + their `*.wants/` symlinks;
   `systemctl daemon-reload`; `umount /var/opt/pwx/oci`; `rm -rf /etc/pwx /opt/pwx
   /var/cache/pwx /var/lib/osd /var/opt/pwx /var/lib/kubelet/plugins/pxd*
   /var/lib/kubelet/plugins_registry/pxd*`.

(A node **reboot** also clears the md/LVM/multipath stack once the backing PVs/iSCSI are gone,
but is more disruptive than the dmsetup teardown.)

## 3. Heal a Degraded MachineConfigPool after node surgery

If you deleted a MachineConfig-managed file (e.g. Portworx ships `99-px-iscsi` which manages
`/etc/systemd/system/px-iscsi-initiatorname.service`), the worker MCP goes **Degraded**
("unexpected on-disk state … could not stat file …") and the MCO won't self-heal it.

- Extract the exact file bytes from the MachineConfig
  (`oc get mc <name> -o json` → `spec.config.systemd.units[].contents`), base64-encode, and
  write it back **byte-identical** on every node; recreate the enable symlink; `daemon-reload`.
- Force revalidation: `oc delete pod -n openshift-machine-config-operator -l k8s-app=machine-config-daemon`.
- Confirm: `oc get mcp worker -o jsonpath='{.status.conditions[?(@.type=="Degraded")].status}'`
  → `False`, `readyMachineCount == machineCount`.

## 4. Clean reset gotchas (fresh install won't provision / storageless deadlock)

After a botched delete + reused cluster name, px often refuses to provision (all nodes go
**storageless**, waiting for a kvdb node that never comes). Root causes and fixes:

- **Stale px metadata ConfigMaps** in `kube-system`: `px-cloud-drive-<clusterid>`,
  `px-bootstrap-<clusterid>`, `px-pure-cloud-drive`, `px-attachdriveset-lock`,
  `px-bringup-queue-lockdefault`. Delete them.
- **Stale node condition** `PortworxNewStorageNodeProvisioned=False / MaxProvisionAttemptsReached`
  (px hit its ~20 attempts before a fix was in place, then stopped retrying). Remove it via a
  status JSON patch, then restart px:
  `oc patch node <n> --subresource=status --type=json -p '[{"op":"remove","path":"/status/conditions/<idx>"}]'`.
- **Stale operator labels** `portworx.io/provision-storage-node[-handled]` — remove so the
  operator re-runs `ApplyProvisionStorageNodeLabels`.
- **Best fix: use a NEW StorageCluster name** for the fresh install. A new name = fresh cluster
  identity = no reused ConfigMap keys / conditions, and the operator requests provisioning
  cleanly (proven reliable). The proper `UninstallAndDelete` wiper (step 1) avoids this whole
  mess in the first place.

## 5. Getting an iterated container image onto nodes without a reachable registry

Build on a host with Docker, push to a small local `registry:2`, then **sideload** onto each
node into CRI-O's shared containers-storage (no cluster registry config / no MCO reboot):

```bash
# on each node:
oc debug node/<n> --quiet -- chroot /host bash -c 'podman pull --tls-verify=false <host:port>/<img>:<tag>'
```

Deployment uses `imagePullPolicy: IfNotPresent` and a **fresh tag per iteration** (so kubelet
picks up the new image). CRI-O and root podman share `/var/lib/containers/storage`, so a
`podman pull` makes the image available to the kubelet.
