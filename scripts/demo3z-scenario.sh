#!/usr/bin/env bash
# 3-zone-via-shim demo: fake 3 arrays from 2 physical FlashArrays so PX's CSI topology accepts an
# odd zone count. fa-a=x50/realm-1/zone-a, fb-b=x20/realm-2/zone-b, fa-c=x20/realm-3/zone-c.
# worker0->zone-a, worker1->zone-b, worker2->zone-c. Runs ON the jump host.
#
#   scripts/demo3z-scenario.sh up      # arrays + 2 shims + DNS (3 FQDNs) + labels + PX (px-3z)
#   scripts/demo3z-scenario.sh down
#   scripts/demo3z-scenario.sh status
set -euo pipefail
REPO="${REPO:-$HOME/px-smt-multirealm-shim}"
export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"
ZONE=/opt/ocp/jumphost/coredns/zones/demo.pure.db
SUFFIX=.ocp4-dev.demo.pure
WORKERS_IP=(10.23.26.25 10.23.26.26 10.23.26.27)
say(){ printf '>> %s\n' "$*"; }
dns_clear(){ sudo sed -i '/fa-a.demo.pure/d;/fb-b.demo.pure/d;/fa-b.demo.pure/d;/fa-c.demo.pure/d;/px-smt 3-zone/d' "$ZONE"; }
dns_bump(){ local c; c=$(awk '/; serial/{print $1; exit}' "$ZONE"); sudo sed -i "s/${c}/$((c+1))/" "$ZONE"; }
dns_reload(){ docker restart ocp-dns >/dev/null; sleep 3; }
sideload(){ docker save "$1" | gzip > /tmp/sl.tgz; for ip in "${WORKERS_IP[@]}"; do scp -o StrictHostKeyChecking=no -q /tmp/sl.tgz core@"$ip":/tmp/sl.tgz; ssh -o StrictHostKeyChecking=no core@"$ip" "sudo podman load -i /tmp/sl.tgz >/dev/null 2>&1; rm -f /tmp/sl.tgz"; done; rm -f /tmp/sl.tgz; }

up_arrays(){ say "provisioning arrays (fa-a/realm-1 on x50; fa-b/realm-2 + fa-c/realm-3 on x20)"
  python3 "$REPO/scripts/fa_provision.py" "$REPO/deploy/demo3z/x50.spec.json"
  python3 "$REPO/scripts/fa_provision.py" "$REPO/deploy/demo3z/x20.spec.json"; }
up_shims(){ say "sideload + deploy shims"
  ( cd "$REPO" && docker build -q -f Dockerfile -t px-shim.local/shim:baseline . >/dev/null ); sideload px-shim.local/shim:baseline
  oc apply -f "$REPO/deploy/demo3z/shims.yaml" >/dev/null
  oc create secret generic px-shim-x50-config -n px-shim --from-file=config.json="$REPO/deploy/demo3z/shim-x50-config.json" --from-literal=array-token="$(cat ~/.x50-token)" --dry-run=client -o yaml | oc apply -f - >/dev/null
  oc create secret generic px-shim-x20-config -n px-shim --from-file=config.json="$REPO/deploy/demo3z/shim-x20-config.json" --from-literal=array-token="$(cat ~/.x20-token)" --dry-run=client -o yaml | oc apply -f - >/dev/null
  oc rollout restart deploy/px-shim-x50 deploy/px-shim-x20 -n px-shim >/dev/null
  oc rollout status deploy/px-shim-x50 -n px-shim --timeout=90s; oc rollout status deploy/px-shim-x20 -n px-shim --timeout=90s; }
up_dns(){ say "DNS -> shim ClusterIPs"
  local i5 i2; i5=$(oc get svc px-shim-x50 -n px-shim -o jsonpath='{.spec.clusterIP}'); i2=$(oc get svc px-shim-x20 -n px-shim -o jsonpath='{.spec.clusterIP}')
  dns_clear
  sudo tee -a "$ZONE" >/dev/null <<EOF

; ---- px-smt 3-zone demo ----
fa-a.demo.pure.   IN  A   $i5
fa-b.demo.pure.   IN  A   $i2
fa-c.demo.pure.   IN  A   $i2
EOF
  dns_bump; dns_reload; say "  fa-a=$i5  fa-b/fa-c=$i2"; }
up_labels(){ say "node zones (worker0=a, worker1=b, worker2=c)"
  oc label node worker0$SUFFIX topology.portworx.io/zone=zone-a --overwrite >/dev/null
  oc label node worker1$SUFFIX topology.portworx.io/zone=zone-b --overwrite >/dev/null
  oc label node worker2$SUFFIX topology.portworx.io/zone=zone-c --overwrite >/dev/null; }
up_px(){ say "deploy PX px-3z"
  oc create namespace portworx --dry-run=client -o yaml | oc apply -f - >/dev/null
  local r1 r2 r3; r1=$(cat ~/.x50-realm-1-token); r2=$(cat ~/.x20-realm-2-token); r3=$(cat ~/.x20-realm-3-token)
  cat > /tmp/pure.json <<EOF
{"FlashArrays":[
 {"MgmtEndPoint":"fa-a.demo.pure","APIToken":"$r1","Realm":"realm-1","Labels":{"topology.portworx.io/zone":"zone-a"}},
 {"MgmtEndPoint":"fa-b.demo.pure","APIToken":"$r2","Realm":"realm-2","Labels":{"topology.portworx.io/zone":"zone-b"}},
 {"MgmtEndPoint":"fa-c.demo.pure","APIToken":"$r3","Realm":"realm-3","Labels":{"topology.portworx.io/zone":"zone-c"}}
]}
EOF
  oc create secret generic px-pure-secret -n portworx --from-file=pure.json=/tmp/pure.json --dry-run=client -o yaml | oc apply -f - >/dev/null; rm -f /tmp/pure.json
  oc apply -f "$REPO/deploy/x50/px-operator-subscription.yaml" >/dev/null
  oc apply -f "$REPO/deploy/demo3z/storagecluster.yaml" >/dev/null; }

down_px(){ say "uninstall px-3z"; oc delete storagecluster px-3z -n portworx --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for i in $(seq 1 48); do oc get storagecluster px-3z -n portworx >/dev/null 2>&1 || { say "  gone"; return 0; }; sleep 10; done; }
down_rest(){ oc delete ns px-shim --ignore-not-found --wait=false >/dev/null 2>&1 || true
  dns_clear; dns_bump; dns_reload
  for n in worker0 worker1 worker2; do oc label node $n$SUFFIX topology.portworx.io/zone- >/dev/null 2>&1 || true; done
  for s in "x50:10.23.26.50:.x50-token" "x20:10.23.26.60:.x20-token"; do t=${s%%:*}; r=${s#*:}; h=${r%%:*}; tf=${r##*:}
    printf '{"host":"%s","token_file":"~/%s","tag":"%s","clean_leftovers":["realm-1","realm-2","realm-3"]}' "$h" "$tf" "$t" > /tmp/c-$t.json
    python3 "$REPO/scripts/fa_provision.py" /tmp/c-$t.json; rm -f /tmp/c-$t.json; done; }

case "${1:-}" in
  up) up_arrays; up_shims; up_dns; up_labels; up_px; say "px-3z up. watch: oc get storagecluster px-3z -n portworx -w" ;;
  down) down_px; down_rest; say "px-3z down." ;;
  status) oc get nodes -L topology.portworx.io/zone --no-headers 2>/dev/null | grep worker | awk '{print $1,$6}'; oc get storagecluster -n portworx 2>/dev/null; oc get deploy -n px-shim 2>/dev/null | grep px-shim ;;
  *) sed -n '2,12p' "${BASH_SOURCE[0]}"; exit 2 ;;
esac
