#!/usr/bin/env bash
set  -euxo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
git clone --depth 1 https://github.com/sourcegraph/deploy-sourcegraph.git

echo "$(pwd)"

ls

NAMESPACE="cluster-ci-$BUILDKITE_BUILD_NUMBER"
kubectl create namespace "$NAMESPACE"
kubectl config set-context --current --namespace="$NAMESPACE"
kubectl get pods

# enter deploy-sourcegraph repo
deploy-sourcegraph/create-new-cluster.sh

kubectl get pods

