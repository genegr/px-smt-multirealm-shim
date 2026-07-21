#!/usr/bin/env bash
# Dev loop for px-smt-multirealm-shim.
#
# The workstation has no Go, no Docker and no rsync; the Linux jumpbox
# (ubuntu@10.23.26.20) has Docker CE and `oc`, but nothing may be installed on it.
# So: source is shipped over `tar | ssh`, and every toolchain invocation runs inside a
# throwaway container on the jumpbox. Nothing lands on the jumpbox except the source
# tree under ~/px-smt-multirealm-shim and Docker images/volumes.
#
#   scripts/devenv.sh setup            # one-time: pull toolchain image, prep cache volumes
#   scripts/devenv.sh sync             # ship the working tree to the jumpbox
#   scripts/devenv.sh build            # sync + compile all three binaries
#   scripts/devenv.sh test [pkgs...]   # sync + go test (default ./...)
#   scripts/devenv.sh vet              # sync + go vet ./...
#   scripts/devenv.sh go <args...>     # sync + arbitrary go command
#   scripts/devenv.sh image <target> [tag]   # build a container image (shim|cleaner)
#   scripts/devenv.sh oc <args...>     # run oc on the jumpbox against the cluster
#   scripts/devenv.sh shell            # interactive shell in the toolchain container
#   scripts/devenv.sh clean            # drop the remote tree and cache volumes
#
# Env overrides: DEV_HOST, DEV_DIR, GO_IMAGE, REGISTRY.
set -euo pipefail

DEV_HOST="${DEV_HOST:-ubuntu@10.23.26.20}"
DEV_DIR="${DEV_DIR:-px-smt-multirealm-shim}"
GO_IMAGE="${GO_IMAGE:-golang:1.23}"
REGISTRY="${REGISTRY:-127.0.0.1:5000}"
GOCACHE_VOL="pxshim-gocache"
GOMOD_VOL="pxshim-gomod"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say() { printf '>> %s\n' "$*" >&2; }

# Run a command on the jumpbox. Quoting is the caller's problem.
remote() { ssh "${DEV_HOST}" "$@"; }

# Run `go ...` (or any command) in the toolchain container, as the ubuntu uid/gid so the
# synced tree never picks up root-owned files the workstation-side sync cannot replace.
go_run() {
  local cmd="$*"
  remote "docker run --rm \
    -u \"\$(id -u):\$(id -g)\" \
    -e HOME=/tmp -e GOCACHE=/gocache -e GOMODCACHE=/gomod -e GOFLAGS=-mod=mod \
    -v \"\$HOME/${DEV_DIR}:/src\" \
    -v ${GOCACHE_VOL}:/gocache \
    -v ${GOMOD_VOL}:/gomod \
    -w /src ${GO_IMAGE} ${cmd}"
}

cmd_setup() {
  say "pulling ${GO_IMAGE} on ${DEV_HOST}"
  remote "docker pull -q ${GO_IMAGE}"
  say "preparing cache volumes (${GOCACHE_VOL}, ${GOMOD_VOL})"
  # Named volumes are created root-owned; chown once so the unprivileged build can write.
  remote "docker volume create ${GOCACHE_VOL} >/dev/null; \
          docker volume create ${GOMOD_VOL} >/dev/null; \
          docker run --rm -v ${GOCACHE_VOL}:/gocache -v ${GOMOD_VOL}:/gomod ${GO_IMAGE} \
            chown -R \"\$(id -u):\$(id -g)\" /gocache /gomod"
  say "setup complete"
}

cmd_sync() {
  say "syncing ${repo_root} -> ${DEV_HOST}:~/${DEV_DIR}"
  tar -C "${repo_root}" \
      --exclude=.git --exclude=scratch --exclude='*.itest' --exclude='*.exe' \
      -czf - . |
    remote "rm -rf ~/${DEV_DIR} && mkdir -p ~/${DEV_DIR} && tar -C ~/${DEV_DIR} -xzf -"
}

cmd_build() {
  cmd_sync
  for c in px-smt-multirealm-shim fada-cleaner pxstress; do
    say "building ${c}"
    go_run "go build -o /tmp/${c} ./cmd/${c}"
  done
  say "all binaries compiled"
}

cmd_test() {
  cmd_sync
  local pkgs="${*:-./...}"
  say "go test ${pkgs}"
  go_run "go test ${pkgs}"
}

cmd_vet() {
  cmd_sync
  say "go vet ./..."
  go_run "go vet ./..."
}

cmd_go() {
  cmd_sync
  go_run "go $*"
}

cmd_image() {
  local target="${1:-shim}" tag="${2:-dev}" dockerfile name
  case "${target}" in
    shim)    dockerfile=Dockerfile;         name=px-shim ;;
    cleaner) dockerfile=Dockerfile.cleaner; name=px-fada-cleaner ;;
    *) echo "unknown image target '${target}' (want: shim|cleaner)" >&2; return 2 ;;
  esac
  cmd_sync
  local ref="${REGISTRY}/${name}:${tag}"
  say "building ${ref} from ${dockerfile}"
  remote "cd ~/${DEV_DIR} && docker build -f ${dockerfile} -t ${ref} ."
  say "built ${ref}"
}

cmd_oc() {
  remote "KUBECONFIG=\$HOME/.kube/config oc $*"
}

cmd_shell() {
  cmd_sync
  ssh -t "${DEV_HOST}" "docker run --rm -it \
    -u \"\$(id -u):\$(id -g)\" \
    -e HOME=/tmp -e GOCACHE=/gocache -e GOMODCACHE=/gomod \
    -v \"\$HOME/${DEV_DIR}:/src\" \
    -v ${GOCACHE_VOL}:/gocache -v ${GOMOD_VOL}:/gomod \
    -w /src ${GO_IMAGE} bash"
}

cmd_clean() {
  say "removing ~/${DEV_DIR} and cache volumes on ${DEV_HOST}"
  remote "rm -rf ~/${DEV_DIR}; docker volume rm -f ${GOCACHE_VOL} ${GOMOD_VOL} >/dev/null"
}

sub="${1:-}"; shift || true
case "${sub}" in
  setup) cmd_setup ;;
  sync)  cmd_sync ;;
  build) cmd_build ;;
  test)  cmd_test "$@" ;;
  vet)   cmd_vet ;;
  go)    cmd_go "$@" ;;
  image) cmd_image "$@" ;;
  oc)    cmd_oc "$@" ;;
  shell) cmd_shell ;;
  clean) cmd_clean ;;
  *) sed -n '3,25p' "${BASH_SOURCE[0]}" >&2; exit 2 ;;
esac
