#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"

tempdir=$(mktemp -d)

export BRIGADE_WORKSPACE="${tempdir}/repo"

check_equal() {
  [[ "$1" == "$2" ]] || {
    echo >&2 "Check failed: '$1' == '$2' ${3:+ ($3)}"
    exit 1
  }
}

setup_git_server() {
  local srvroot="${root_dir}/tmp"

  git ls-remote git://localhost/test.git >/dev/null 2>&1 || git daemon --base-path="${srvroot}" --export-all --reuseaddr "${srvroot}" >/dev/null 2>&1 &

  sleep 5

  [[ -d  "${srvroot}" ]] && return

  (
  unset XDG_CONFIG_HOME
  export HOME=/dev/null
  export GIT_CONFIG_NOSYSTEM=1

  repo_root="${root_dir}/tmp/test.git"

  # TODO: update such that this url can be overridden by tests
  git clone --mirror https://github.com/brigadecore/empty-testbed.git "${repo_root}"
  )
}

cleanup() {
  pkill -9 git-daemon >/dev/null 2>&1
  rm -rf "${tempdir}"
}
# trap 'cleanup' EXIT

test_clone() {
  local revision="$1" want="$2"

  jq -n \
    --arg ref "${revision}" \
    --arg cloneURL "git://127.0.0.1/test.git" \
    '{worker: {git: {ref: $ref, cloneURL: $cloneURL}}}' > ./event.json

  cat ./event.json | jq

  ../../bin/vcs-sidecar \
    -p ./event.json \
    -w "${BRIGADE_WORKSPACE}" # \
    # --sshKey ./testkey.pem

  # TODO: this used to check FETCH_HEAD; using go-git we don't see this symbolic ref
  got="$(git -C ${BRIGADE_WORKSPACE} rev-parse --short HEAD)"

  check_equal "${want}" "${got}"

  rm -rf "${BRIGADE_WORKSPACE}"
}

setup_git_server

echo ":: Checkout tag"
test_clone "v0.1.0" "ddff78a"
echo

echo ":: Checkout branch"
test_clone "hotfix" "589e150"
echo

echo ":: Checkout pull request by reference"
test_clone "refs/pull/1/head" "5c4bc10"
echo

echo "All tests passing"
