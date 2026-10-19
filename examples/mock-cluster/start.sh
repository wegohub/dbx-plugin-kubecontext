#!/usr/bin/env bash
# Start the fake Kubernetes API server used by the kubeconfig in this folder.
# It answers /version, namespaces, nodes, pods, pod logs, events, PATCH and DELETE,
# so the whole workbench can be exercised without a real cluster.
set -euo pipefail
cd "$(dirname "$0")"
PORT="${1:-18080}"
echo "mock Kubernetes API listening on http://127.0.0.1:${PORT}"
exec python3 mock_kube_api.py "$PORT"
