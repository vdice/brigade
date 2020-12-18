#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
srvroot="${root_dir}/tmp"

tempdir=$(mktemp -d)

export BRIGADE_WORKSPACE="${tempdir}/repo"

check_equal() {
  [[ "$1" == "$2" ]] || {
    echo >&2 "Check failed: '$1' == '$2' ${3:+ ($3)}"
    exit 1
  }
}

master_sha="${tempdir}/master.sha"
full_master_sha="${tempdir}/master.sha.full"
tag_sha="${tempdir}/tag.sha"
hotfix_sha="${tempdir}/hotfix.sha"

setup_git_server() {
  # Is there a way to enable exact SHA matching on this server?
  # Something like:
  # --enable="allow-reachable-sha1-in-want" (no, this doesn't work)
  git ls-remote git://localhost/test.git >/dev/null 2>&1 || \
    git daemon --base-path="${srvroot}" --export-all --reuseaddr "${srvroot}" >/dev/null 2>&1 &

  sleep 5

  (
  unset XDG_CONFIG_HOME
  export HOME=/dev/null
  export GIT_CONFIG_NOSYSTEM=1

  repo_root="${srvroot}/test.git"

  mkdir -p "${repo_root}"
  cd "${repo_root}"

  # init
  git init >/dev/null 2>&1

  # first commit
  echo "This is a test repo, y'all" > README.md
  git add . >/dev/null 2>&1
  git commit -m "Add README.md" >/dev/null 2>&1 || true
  git rev-parse --short master > "${master_sha}"
  git rev-parse master > "${full_master_sha}"

  # create tag
  git tag -a -m "This is a tag, y'all" v0.1.0 >/dev/null 2>&1 || true
  git rev-parse --short v0.1.0 > "${tag_sha}"

  # create branch
  git checkout -b hotfix >/dev/null 2>&1 || true
  echo "HOTFIX!" > README.md
  git add . >/dev/null 2>&1
  git commit -m "Hotfix README.md" >/dev/null 2>&1 || true
  git rev-parse --short hotfix > "${hotfix_sha}"
  )

}

cleanup() {
  pkill -9 git-daemon >/dev/null 2>&1
  rm -rf "${srvroot}"
  rm -rf "${tempdir}"
}
trap 'cleanup' EXIT

test_clone() {
  local revision="$1" want="$2"
  local eventjson="${tempdir}/event.json"

  local key="ref"
  if [[ ${#revision} == 40 ]]; then
    key="commit"
  elif [[ ${#revision} == 0 ]]; then
    # switch key to something bogus so that no ref or commit is included
    key="foo"
  fi

  jq -n \
    --arg ref "${revision}" \
    --arg cloneURL "git://127.0.0.1/test.git" \
    '{worker: {git: {'"${key}"': $ref, cloneURL: $cloneURL}}}' > "${eventjson}"

  cat "${eventjson}" | jq

  ../../bin/vcs-sidecar \
    -p "${eventjson}" \
    -w "${BRIGADE_WORKSPACE}"

  got="$(git -C ${BRIGADE_WORKSPACE} rev-parse --short FETCH_HEAD)"

  check_equal "${want}" "${got}"

  rm -rf "${BRIGADE_WORKSPACE}"
}

setup_git_server

# TODO: add fail cases (at least no ref found)

# TODO: update tests for more configurability, e.g.
# # using private repo url (ssh key)
# # using repo with git submodules to init

echo ":: Checkout master"
test_clone "master" "$(cat ${master_sha})"
echo

echo ":: Checkout w/o supplying ref or commit" # should return master commit
test_clone "" "$(cat ${master_sha})"
echo

# Ugh: error fetching refs from the remote: server does not support exact SHA1 refspec
# echo ":: Checkout sha"
# test_clone "$(cat ${full_master_sha})" "$(cat ${master_sha})"
# echo

echo ":: Checkout tag"
test_clone "v0.1.0" "$(cat ${tag_sha})"
echo

echo ":: Checkout branch"
test_clone "hotfix" "$(cat ${hotfix_sha})"
echo

# TODO: not sure how to test PR branches... need GitHub?
# echo ":: Checkout pull request by reference"
# test_clone "refs/pull/1/head" "5c4bc10"
# echo

echo "All tests passing"
