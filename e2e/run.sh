#!/bin/bash
# custom script for e2e testing
# ~/bin must exist and be part of $PATH

# kudos to https://elder.dev/posts/safer-bash/
set -o errexit # script exits when a command fails == set -e
set -o nounset # script exits when tries to use undeclared variables == set -u
#set -o xtrace # trace what's executed == set -x (useful for debugging)
set -o pipefail # causes pipelines to retain / set the last non-zero status

KUBECTL_PLATFORM=linux/amd64
KUBECTL_VERSION=v1.15.0
KUBECTL_EXECUTABLE=kubectl

KIND_PLATFORM=kind-linux-amd64
KIND_VERSION=v0.4.0
KIND_EXECUTABLE=kind

HELM_PLATFORM=linux-amd64
HELM_VERSION=helm-v3.0.0-alpha.1
HELM_EXECUTABLE=helm3

if [ -z "$BRIG_IMAGE" ]
then
      echo "\$BRIG_IMAGE is empty, it must be set before running this script"
      exit 1
fi

# check if kubectl is installed
if ! [ -x "$(command -v kubectl)" ]; then
  echo 'Error: kubectl is not installed. Installing...'
  curl -LO https://storage.googleapis.com/kubernetes-release/release/$KUBECTL_VERSION/bin/$KUBECTL_PLATFORM/kubectl && chmod +x ./kubectl && mv kubectl ~/bin/$KUBECTL_EXECUTABLE
fi

# check if kind is installed
if ! [ -x "$(command -v $KIND_EXECUTABLE)" ]; then
    echo 'Error: kind is not installed. Installing...'
    wget https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/$KIND_PLATFORM && mv $KIND_PLATFORM ~/bin/$KIND_EXECUTABLE && chmod +x ~/bin/$KIND_EXECUTABLE
fi

# check if helm is installed
if ! [ -x "$(command -v $HELM_EXECUTABLE)" ]; then
    echo 'Error: Helm is not installed. Installing...'
    wget https://get.helm.sh/$HELM_VERSION-$HELM_PLATFORM.tar.gz && tar -xvzf $HELM_VERSION-$HELM_PLATFORM.tar.gz && rm -rf $HELM_VERSION-$HELM_PLATFORM.tar.gz && mv $HELM_PLATFORM/helm ~/bin/$HELM_EXECUTABLE && chmod +x ~/bin/$HELM_EXECUTABLE
fi

# create kind k8s cluster
$KIND_EXECUTABLE create cluster

function finish {
  echo "-----Cleaning up-----"
  $KIND_EXECUTABLE delete cluster
}

trap finish EXIT

# set KUBECONFIG with details from kind
export KUBECONFIG="$($KIND_EXECUTABLE get kubeconfig-path --name="kind")"

# build all images and load them onto kind
DOCKER_ORG=brigadecore make build-all-images load-all-images

# init helm
$HELM_EXECUTABLE init

# add brigade chart repo
$HELM_EXECUTABLE repo add brigade https://brigadecore.github.io/charts

# install the images onto kind cluster
HELM=$HELM_EXECUTABLE make helm-install

echo "-----Waiting for Brigade API server and Controller deployments-----"
# https://stackoverflow.com/questions/59895/getting-the-source-directory-of-a-bash-script-from-within
DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null && pwd )"
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-api
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-cr-gw
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-ctrl
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-generic-gateway
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-github-app
"${DIR}"/wait-for-deployment.sh -n default brigade-server-brigade-github-oauth
"${DIR}"/wait-for-deployment.sh -n default brigade-server-kashti

echo "-----Creating  a test project-----"
go run "${DIR}"/../brig/cmd/brig/main.go project create -f "${DIR}"/testproject.yaml

echo "-----Checking if the test project secret was created-----"
PROJECT_NAME=$($KUBECTL_EXECUTABLE get secret -l app=brigade,component=project,heritage=brigade | tail -n 1 | cut -f 1 -d ' ')
if [ $PROJECT_NAME != "brigade-5b55ed522537b663e178f751959d234fd650d626f33f70557b2e82" ]; then
    echo "Wrong secret name. Expected brigade-5b55ed522537b663e178f751959d234fd650d626f33f70557b2e82, got $PROJECT_NAME"
    exit 1
fi
