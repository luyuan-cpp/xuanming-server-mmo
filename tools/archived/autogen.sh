#!/bin/bash
#
# autogen.sh — thin wrapper kept for backwards compatibility.
#
# The Linux build now lives in tools/scripts/build_linux.sh, which is also what
# deploy/k8s/Dockerfile.cpp invokes. This file used to carry its own copy of the
# project build order; two copies would drift apart the moment either side
# gained a target, so the list now exists only in build_linux.sh.
#
# Original behaviour (submodules + deps + generate + Release build) is preserved
# by forwarding the default flags.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

echo "autogen.sh: delegating to tools/scripts/build_linux.sh --release"
exec bash "$REPO_ROOT/tools/scripts/build_linux.sh" --release "$@"
