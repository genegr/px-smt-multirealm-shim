#!/usr/bin/env bash
# Build the px-shim image on the dev host and push it to the demo registry.
#
# Topology: workstation --ssh--> jump host (<ssh-user>@<jump-host-ip>) --ssh--> dev host
# (<ssh-user>@<dev-host-ip>, which has Docker + registry login). This script tars the repo,
# relays it to the dev host through the jump host, then runs the multi-stage Docker build
# and push there.
set -euo pipefail

JUMP="${JUMP:-<ssh-user>@<jump-host-ip>}"
DEV="${DEV:-<ssh-user>@<dev-host-ip>}"
REGISTRY="${REGISTRY:-registry.demo.pure:30005}"
IMAGE="${IMAGE:-px-shim}"
TAG="${TAG:-dev}"
REF="${REGISTRY}/${IMAGE}:${TAG}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
echo ">> packaging ${repo_root}"
tar -C "${repo_root}" --exclude=.git --exclude=scratch -czf - . |
  ssh "${JUMP}" "ssh ${DEV} 'rm -rf ~/px-shim-build && mkdir -p ~/px-shim-build && tar -C ~/px-shim-build -xzf -'"

echo ">> building ${REF} on the dev host"
ssh "${JUMP}" "ssh ${DEV} 'cd ~/px-shim-build && docker build -t ${REF} . && docker push ${REF}'"

echo ">> done: ${REF}"
