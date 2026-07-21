#!/usr/bin/env bash
# Emit the OpenShift MachineConfig that installs the Pure FlashArray FC multipath.conf +
# udev rules on worker nodes and enables multipathd. Reads the two source files under
# deploy/pure-fc/ and base64-embeds them, so the applied MC always matches the repo.
#
#   scripts/fc-machineconfig.sh            # print the MachineConfig YAML to stdout
#   scripts/fc-machineconfig.sh | oc apply -f -   # (run where oc + cluster are reachable)
#
# FC-only: enables multipathd.service but NOT iscsid (no iSCSI on these nodes).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Strip any CR (files may be authored on Windows) so the on-node files are clean LF.
mp_b64="$(tr -d '\r' < "${here}/deploy/pure-fc/multipath.conf" | base64 -w0)"
udev_b64="$(tr -d '\r' < "${here}/deploy/pure-fc/99-pure-storage.rules" | base64 -w0)"

cat <<YAML
apiVersion: machineconfiguration.openshift.io/v1
kind: MachineConfig
metadata:
  labels:
    machineconfiguration.openshift.io/role: worker
  name: 99-worker-pure-fc-multipath
spec:
  config:
    ignition:
      version: 3.2.0
    storage:
      files:
      - contents:
          source: data:text/plain;charset=utf-8;base64,${mp_b64}
        mode: 0644
        overwrite: true
        path: /etc/multipath.conf
      - contents:
          source: data:text/plain;charset=utf-8;base64,${udev_b64}
        mode: 0644
        overwrite: true
        path: /etc/udev/rules.d/99-pure-storage.rules
    systemd:
      units:
      - enabled: true
        name: multipathd.service
YAML
