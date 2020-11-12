#!/usr/bin/env bash
set  -euxo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
git clone --depth 1 https://github.com/sourcegraph/deploy-sourcegraph.git

echo "$(pwd)"
ls

#NAMESPACE="cluster-ci-$BUILDKITE_BUILD_NUMBER"
# TODO(Dax): Buildkite cannot create namespaces at cluster level
NAMESPACE=cluster-ci-121
#kubectl create namespace "$NAMESPACE"
kubectl config set-context --current --namespace="$NAMESPACE"
kubectl get pods

# enter deploy-sourcegraph repo
deploy-sourcegraph/create-new-cluster.sh

kubectl get pods
time kubectl wait  --for=condition=Ready -l app=sourcegraph-frontend pod \
  --timeout=5m

LOGFILE=frontend-logs
# kubectl logs
kubectl_logs() {
  echo "Appending frontend logs"
  kubectl logs  -l "app=sourcegraph-frontend" -c frontend >> $LOGFILE.log
  chmod 744 $LOGFILE.log
  #kubectl delete namespace $NAMESPACE
}
trap kubectl_logs EXIT

set -x

test/setup-deps.sh
test/setup-display.sh

sleep 15
SOURCEGRAPH_URL=http://sourcegraph-frontend.dogfood-k8s.svc.cluster.local:30080
curl $SOURCEGRAPH_URL

# setup admin users, etc
go run ../init-server.go -base-url=$SOURCEGRAPH_URL

# Load variables set up by init-server, disabling `-x` to avoid printing variables
set +x
# shellcheck disable=SC1091
source /root/.profile
set -x

echo "TEST: Checking Sourcegraph instance is accessible"

curl --fail $SOURCEGRAPH_URL
curl --fail "$SOURCEGRAPH_URL/healthz"
echo "TEST: Running tests"
pushd client/web || exit
yarn run test:regression:core

popd || exit

kubectl get pods
