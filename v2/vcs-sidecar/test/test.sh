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

# TODO: remove or update; we've opted to supply GitHub URLs directly in the test_clone method for now...
# # areese mentioned updating to serve a fresh, local repo and then creating objects (create/push branches, tags, etc.)
# # Then, we won't have to clone an external repo
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

  git clone --mirror https://github.com/brigadecore/empty-testbed.git "${repo_root}"
  )
}

cleanup() {
  # These were from the deactivated local git server
  # pkill -9 git-daemon >/dev/null 2>&1
  # rm -rf "${tempdir}"
  rm "${root_dir}/event.json"
}
trap 'cleanup' EXIT

test_clone() {
  local revision="$1" want="$2"
  local eventjson="${root_dir}/event.json"

  local key="ref"
  if [[ ${#revision} == 40 ]]; then
    key="commit"
  elif [[ ${#revision} == 0 ]]; then
    key="foo"
  fi

  jq -n \
    --arg ref "${revision}" \
    --arg cloneURL "https://github.com/brigadecore/empty-testbed.git" \
    '{worker: {git: {'"${key}"': $ref, cloneURL: $cloneURL}}}' > "${eventjson}"

  cat "${eventjson}" | jq

  ../../bin/vcs-sidecar \
    -p "${eventjson}" \
    -w "${BRIGADE_WORKSPACE}" # \
    # --sshKey ./testkey.pem

  got="$(git -C ${BRIGADE_WORKSPACE} rev-parse --short FETCH_HEAD)"

  check_equal "${want}" "${got}"

  rm -rf "${BRIGADE_WORKSPACE}"
}

# setup_git_server

# TODO: add fail cases (at least no ref found)

# TODO: update tests for more configurability, e.g.
# # using private repo url (ssh key)
# # using repo with git submodules to init

echo ":: Checkout sha"
test_clone "99f3efa2b70c370d4ee0833c213c085a6ec146ab" "99f3efa"
echo

echo ":: Checkout tag"
test_clone "v0.1.0" "ddff78a"
echo

echo ":: Checkout branch"
test_clone "hotfix" "589e150"
echo

echo ":: Checkout pull request by reference"
test_clone "refs/pull/1/head" "5c4bc10"
echo

echo ":: Checkout w/o supplying ref or commit" # should return master commit
test_clone "" "1ecc6f4"
echo

echo "All tests passing"
