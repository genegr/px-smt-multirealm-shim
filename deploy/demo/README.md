# Demo scenario (scenario B) + nested OpenShift on KubeVirt

An "easier" demo built on the same two FlashArrays, **without the shim**, plus a nested compact
OpenShift cluster running in KubeVirt VMs. Runs on the `ocp4-dev` host cluster. To return to the
shim/topology setup afterwards, run `scripts/topo-scenario.sh up` (scenario A).

## 1. Arrays (no shim)

The array-level FC hosts are **deleted** so Portworx creates its own realm hosts and claims the
node WWNs natively (that removal is why no shim is needed):

```
# per array, via scripts/fa_provision.py with a spec that has clean_leftovers + remove_hosts
{"host":"10.23.26.50","token_file":"~/.x50-token","tag":"x50",
 "clean_leftovers":["realm-1","realm-2"],"remove_hosts":["worker0"]}
{"host":"10.23.26.60","token_file":"~/.x20-token","tag":"x20",
 "clean_leftovers":["realm-1","realm-2"],"remove_hosts":["worker1","worker2"]}
```

## 2. Portworx (single realm, no shim)

`px-pure-secret` `pure.json` has two entries, both `Realm: realm-1`, `MgmtEndPoint` = the array
**IPs directly** (10.23.26.50 / 10.23.26.60). We first tried CSI topology for a 2-KVDB-in-one-zone
/ 1-in-the-other layout, but the operator **refuses a 2-zone cluster** ("exactly 2 zones ... quorum
loss"), so the topology is dropped: `deploy/demo/storagecluster-notopo.yaml` (px-demo) runs
single-domain, FC, KVDB across all 3 nodes. (`storagecluster.yaml` is the rejected topology variant,
kept for the record.)

## 3. OpenShift Virtualization

`deploy/demo/openshift-virtualization.yaml` (operator) then `deploy/demo/hyperconverged.yaml`.

## 4. VM networking

`deploy/demo/vm-network.yaml` — kubernetes-nmstate builds `br-vm` on each worker's spare NIC
`eno34np1` (on 10.23.26.0/24), and a `cnv-bridge` NAD in namespace `mini`. (Install the
`kubernetes-nmstate-operator` first.)

## 5. Nested compact cluster "mini" (Agent-based installer)

3 VMs, each a schedulable control-plane node; `platform: baremetal` with API VIP `10.23.26.34`
and Ingress VIP `10.23.26.35`; base domain `demo.pure` → `api.mini.demo.pure`,
`*.apps.mini.demo.pure` (served by the jump-host coredns). Nodes are static: mini-0/1/2 =
`10.23.26.31/.32/.33`, pinned by MAC `02:00:00:aa:00:31/32/33`.

Build (on the jump host, in a self-contained `~/nested` workdir — nothing installed system-wide):

1. `openshift-install` 4.20 downloaded to `~/nested/bin`.
2. `install-config.yaml` + `agent-config.yaml` generated (pull secret from the host cluster's
   `openshift-config/pull-secret`, ssh key `~/.ssh/id_rsa.pub`; static IPs via nmstate per host).
3. Generate the agent ISO **inside a `centos:stream9` container** (it needs `nmstatectl` + `oc`,
   which the jump host lacks): `docker run --rm -v ~/nested:/nested -v /usr/local/bin/oc:/usr/bin/oc:ro
   quay.io/centos/centos:stream9 bash -c 'dnf -y install nmstate; /nested/bin/openshift-install agent create image --dir /nested/work'`.
4. Serve the ISO from the jump-host nginx (`/opt/ocp/jumphost/nginx/www`, port 8080).
5. `oc apply -f deploy/demo/mini-vms.yaml` — CDI imports the ISO per VM (cdrom) + a blank 120Gi
   PX-replicated root disk; boot order root=1/iso=2 so the empty disk falls through to the ISO
   installer, then boots the installed disk on reboot.
6. `openshift-install agent wait-for install-complete --dir ~/nested/work`.

Kubeconfig lands at `~/nested/work/auth/kubeconfig`.
