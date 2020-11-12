#!/usr/bin/env bash

# shellcheck disable=SC1091
source /root/.profile
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit

set -x

dev/ci/test/setup-deps.sh
dev/ci/test/setup-display.sh

# ==========================

pushd enterprise || exit
./cmd/server/pre-build.sh
./cmd/server/build.sh
popd || exit
./dev/ci/e2e.sh
docker image rm -f "${IMAGE}"

# ==========================

dev/ci/test/cleanup-display.sh
