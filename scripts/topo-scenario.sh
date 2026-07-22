#!/usr/bin/env bash
# Rebuild or tear down "scenario A": the multi-array CSI-topology + per-array-shim FADA setup
# (x50=zone-a, x20=zone-b; same-named realms; PX px-topo; worker0=zone-a, worker1=zone-b,
# worker2=zone-c). Idempotent enough to run clean -> up, and up after a down (rollback).
#
# Runs ON the jump host. Needs: oc (KUBECONFIG), docker, ~/fa.sh, the realm/array token files
# under ~ (persisted from provisioning), and write access to the coredns zone via sudo.
#
#   scripts/topo-scenario.sh up      # arrays(idempotent) + shims + DNS + node labels + PX [+ cleaner]
#   scripts/topo-scenario.sh down    # PX uninstall + cleaner + shims + DNS + labels + array cleanup
#   scripts/topo-scenario.sh status
#
# Env: REPO (default ~/px-smt-multirealm-shim), CLEANER=true|false (deploy fada-cleaner in up).
set -euo pipefail

REPO="${REPO:-$HOME/px-smt-multirealm-shim}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
ZONE=/opt/ocp/jumphost/coredns/zones/demo.pure.db
CLEANER="${CLEANER:-true}"
SUFFIX=.ocp4-dev.demo.pure
WORKERS_IP=(10.23.26.25 10.23.26.26 10.23.26.27)
NODES=(worker0 worker1 worker2)
ZONES=(zone-a zone-b zone-c)   # index-aligned with NODES

say(){ printf '>> %s\n' "$*"; }

dns_clear(){ sudo sed -i '/x50-1a.demo.pure/d;/x50-2a.demo.pure/d;/x20-1b.demo.pure/d;/x20-2b.demo.pure/d;/px-smt topology/d' "$ZONE"; }
dns_bump(){ local c; c=$(awk '/; serial/{print $1; exit}' "$ZONE"); sudo sed -i "s/${c}/$((c+1))/" "$ZONE"; }
dns_reload(){ docker restart ocp-dns >/dev/null; sleep 3; }

sideload(){ # <image>
  local img="$1" f=/tmp/sideload.tar.gz
  docker save "$img" | gzip > "$f"
  for ip in "${WORKERS_IP[@]}"; do
    scp -o StrictHostKeyChecking=no -q "$f" core@"$ip":/tmp/sideload.tar.gz
    ssh -o StrictHostKeyChecking=no core@"$ip" "sudo podman load -i /tmp/sideload.tar.gz >/dev/null 2>&1; rm -f /tmp/sideload.tar.gz"
  done
  rm -f "$f"
}

# -------- UP --------
up_arrays(){ say "provisioning arrays (idempotent)"
  python3 "$REPO/scripts/fa_provision.py" "$REPO/deploy/topology/x50.spec.json"
  python3 "$REPO/scripts/fa_provision.py" "$REPO/deploy/topology/x20.spec.json"; }

up_shims(){ say "building + sideloading shim image"
  ( cd "$REPO" && docker build -f Dockerfile -t px-shim.local/shim:baseline . >/dev/null )
  sideload px-shim.local/shim:baseline
  say "deploying shims"
  oc apply -f "$REPO/deploy/topology/shims.yaml" >/dev/null
  oc create secret generic px-shim-x50-config -n px-shim \
    --from-file=config.json="$REPO/deploy/topology/shim-x50-config.json" \
    --from-literal=array-token="$(cat ~/.x50-token)" --dry-run=client -o yaml | oc apply -f - >/dev/null
  oc create secret generic px-shim-x20-config -n px-shim \
    --from-file=config.json="$REPO/deploy/topology/shim-x20-config.json" \
    --from-literal=array-token="$(cat ~/.x20-token)" --dry-run=client -o yaml | oc apply -f - >/dev/null
  oc rollout restart deploy/px-shim-x50 deploy/px-shim-x20 -n px-shim >/dev/null
  oc rollout status deploy/px-shim-x50 -n px-shim --timeout=90s
  oc rollout status deploy/px-shim-x20 -n px-shim --timeout=90s; }

up_dns(){ say "wiring DNS to shim ClusterIPs"
  local ip50 ip20
  ip50=$(oc get svc px-shim-x50 -n px-shim -o jsonpath='{.spec.clusterIP}')
  ip20=$(oc get svc px-shim-x20 -n px-shim -o jsonpath='{.spec.clusterIP}')
  dns_clear
  sudo tee -a "$ZONE" >/dev/null <<EOF

; ---- px-smt topology: per-array shims ----
x50-1a.demo.pure.   IN  A   $ip50
x50-2a.demo.pure.   IN  A   $ip50
x20-1b.demo.pure.   IN  A   $ip20
x20-2b.demo.pure.   IN  A   $ip20
EOF
  dns_bump; dns_reload
  say "  x50 shim=$ip50  x20 shim=$ip20"; }

up_labels(){ say "labeling nodes with topology zones"
  for i in "${!NODES[@]}"; do oc label node "${NODES[$i]}$SUFFIX" topology.portworx.io/zone="${ZONES[$i]}" --overwrite >/dev/null; done; }

up_px(){ say "deploying PX (px-topo)"
  oc create namespace portworx --dry-run=client -o yaml | oc apply -f - >/dev/null
  local r1a r2a r1b r2b
  r1a=$(cat ~/.x50-realm-1-token); r2a=$(cat ~/.x50-realm-2-token)
  r1b=$(cat ~/.x20-realm-1-token); r2b=$(cat ~/.x20-realm-2-token)
  cat > /tmp/pure.json <<EOF
{"FlashArrays":[
 {"MgmtEndPoint":"x50-1a.demo.pure","APIToken":"$r1a","Realm":"realm-1","Labels":{"topology.portworx.io/zone":"zone-a"}},
 {"MgmtEndPoint":"x50-2a.demo.pure","APIToken":"$r2a","Realm":"realm-2","Labels":{"topology.portworx.io/zone":"zone-a"}},
 {"MgmtEndPoint":"x20-1b.demo.pure","APIToken":"$r1b","Realm":"realm-1","Labels":{"topology.portworx.io/zone":"zone-b"}},
 {"MgmtEndPoint":"x20-2b.demo.pure","APIToken":"$r2b","Realm":"realm-2","Labels":{"topology.portworx.io/zone":"zone-b"}}
]}
EOF
  oc create secret generic px-pure-secret -n portworx --from-file=pure.json=/tmp/pure.json --dry-run=client -o yaml | oc apply -f - >/dev/null
  rm -f /tmp/pure.json
  oc apply -f "$REPO/deploy/x50/px-operator-subscription.yaml" >/dev/null
  oc apply -f "$REPO/deploy/topology/storagecluster.yaml" >/dev/null; }

up_cleaner(){ [ "$CLEANER" = true ] || return 0; say "deploying fada-cleaner"
  ( cd "$REPO" && docker build -f Dockerfile.cleaner -t px-fada-cleaner.local/cleaner:v1 . >/dev/null )
  sideload px-fada-cleaner.local/cleaner:v1
  oc apply -f "$REPO/deploy/topology/fada-cleaner.yaml" >/dev/null; }

# -------- DOWN --------
down_px(){ say "uninstalling PX (UninstallAndDelete)"
  oc delete storagecluster px-topo -n portworx --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for i in $(seq 1 48); do oc get storagecluster px-topo -n portworx >/dev/null 2>&1 || { say "  PX gone"; return 0; }; sleep 10; done
  say "  WARN: PX still uninstalling after 8m"; }

flush_maps(){ say "flushing stale multipath maps (cleaner best-effort)"
  if oc get ns px-fada-cleaner >/dev/null 2>&1; then sleep 45; fi; }

down_cleaner(){ oc delete -f "$REPO/deploy/topology/fada-cleaner.yaml" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
down_shims(){ say "removing shims"; oc delete ns px-shim --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
down_dns(){ say "removing DNS records"; dns_clear; dns_bump; dns_reload; }
down_labels(){ say "removing node zone labels"; for n in "${NODES[@]}"; do oc label node "$n$SUFFIX" topology.portworx.io/zone- >/dev/null 2>&1 || true; done; }
down_arrays(){ say "cleaning array leftovers (keep realms/pods/hosts/tokens)"
  for spec in "x50:10.23.26.50:.x50-token" "x20:10.23.26.60:.x20-token"; do
    local tag=${spec%%:*} rest=${spec#*:} host tf; host=${rest%%:*}; tf=${rest##*:}
    cat > /tmp/clean-$tag.json <<EOF
{"host":"$host","token_file":"~/.$tf","tag":"$tag","clean_leftovers":["realm-1","realm-2"]}
EOF
    python3 "$REPO/scripts/fa_provision.py" /tmp/clean-$tag.json; rm -f /tmp/clean-$tag.json
  done; }

do_status(){
  echo "== nodes/zones =="; oc get nodes -L topology.portworx.io/zone --no-headers 2>/dev/null | grep worker | awk '{print $1,$6}'
  echo "== shims =="; oc get deploy,svc -n px-shim 2>/dev/null | grep px-shim || echo "  (none)"
  echo "== DNS =="; for f in x50-1a x50-2a x20-1b x20-2b; do echo -n "  $f.demo.pure -> "; nslookup $f.demo.pure 10.23.26.20 2>/dev/null | awk '/^Address: /{print $2}' | tail -1 || echo "(none)"; done
  echo "== PX =="; oc get storagecluster px-topo -n portworx --no-headers 2>/dev/null || echo "  (none)"
  echo "== cleaner =="; oc get ds -n px-fada-cleaner 2>/dev/null | grep px-fada-cleaner || echo "  (none)"
}

case "${1:-}" in
  up)   up_arrays; up_shims; up_dns; up_labels; up_px; up_cleaner
        say "scenario A up. Watch: oc get storagecluster px-topo -n portworx -w" ;;
  down) down_px; flush_maps; down_cleaner; down_shims; down_dns; down_labels; down_arrays
        say "scenario A down." ;;
  status) do_status ;;
  *) sed -n '2,17p' "${BASH_SOURCE[0]}"; exit 2 ;;
esac
