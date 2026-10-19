#!/usr/bin/env python3
"""Fake Kubernetes API server for the KubeContext plugin demo.

Two credentials, to mirror what ops actually hands out:

  mock-token             cluster-admin-ish, sees everything, may mutate
  mock-restricted-token  a namespace-scoped ServiceAccount: list/get pods and
                         deployments in `default`, read pod logs, list
                         namespaces, no nodes, no cross-namespace, no writes

It implements /version, namespaces, nodes, pods, pod logs, events,
deployments/services/configmaps, keeps scale/delete mutations in memory, and
answers SelfSubjectRulesReview / SelfSubjectAccessReview / SelfSubjectReview so
the workbench permission panel has real data.
"""
import json
import sys
import os
import time
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

LOCK = threading.Lock()
NOW = "2026-09-01T10:00:00Z"

ADMIN_TOKEN = "mock-token"
RESTRICTED_TOKEN = "mock-restricted-token"
RESTRICTED_NAMESPACE = "default"

# Rules returned by SelfSubjectRulesReview for the restricted credential.
# Cluster-scoped resources are deliberately absent: the real API never returns
# them here, which is why the plugin probes them with SelfSubjectAccessReview.
RESTRICTED_RULES = [
    {"verbs": ["get", "list", "watch"], "apiGroups": [""], "resources": ["pods"], "resourceNames": []},
    {"verbs": ["get"], "apiGroups": [""], "resources": ["pods/log"], "resourceNames": []},
    {"verbs": ["get", "list"], "apiGroups": [""], "resources": ["services", "configmaps"], "resourceNames": []},
    {"verbs": ["get", "list"], "apiGroups": ["apps"], "resources": ["deployments"], "resourceNames": []},
]
ADMIN_RULES = [
    {"verbs": ["*"], "apiGroups": ["*"], "resources": ["*"], "resourceNames": []},
]

PODS = [
    {
        "metadata": {"name": "web-7d9f8c6b5-abcde", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"nodeName": "node-a", "containers": [{"name": "nginx", "image": "nginx:1.27"}]},
        "status": {"phase": "Running", "containerStatuses": [{"name": "nginx", "ready": True, "restartCount": 0, "state":{"running":{}}}]},
    },
    {
        "metadata": {"name": "api-6c4b7d9f8-fghij", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"nodeName": "node-b", "containers": [{"name": "api", "image": "ghcr.io/example/api:1.4.2"}]},
        "status": {"phase": "Running", "containerStatuses": [{"name": "api", "ready": False, "restartCount": 7}]},
    },
    {
        "metadata": {"name": "coredns-5d78c9869d-klmno", "namespace": "kube-system", "creationTimestamp": NOW},
        "spec": {"nodeName": "node-a", "containers": [{"name": "coredns", "image": "coredns/coredns:1.11.1"}]},
        "status": {"phase": "Running", "containerStatuses": [{"name": "coredns", "ready": True, "restartCount": 0}]},
    },
]

DEPLOYMENTS = [
    {
        "metadata": {"name": "web", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"replicas": 3, "template": {"spec": {"containers": [{"image": "nginx:1.27"}]}}},
        "status": {"replicas": 3, "readyReplicas": 3},
    },
    {
        "metadata": {"name": "api", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"replicas": 2, "template": {"spec": {"containers": [{"image": "ghcr.io/example/api:1.4.2"}]}}},
        "status": {"replicas": 2, "readyReplicas": 1},
    },
    {
        "metadata": {"name": "coredns", "namespace": "kube-system", "creationTimestamp": NOW},
        "spec": {"replicas": 2, "template": {"spec": {"containers": [{"image": "coredns/coredns:1.11.1"}]}}},
        "status": {"replicas": 2, "readyReplicas": 2},
    },
]

SERVICES = [
    {
        "metadata": {"name": "web", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"type": "NodePort", "clusterIP": "10.96.0.11", "ports": [{"port": 80}]},
    },
    {
        "metadata": {"name": "kubernetes", "namespace": "default", "creationTimestamp": NOW},
        "spec": {"type": "ClusterIP", "clusterIP": "10.96.0.1", "ports": [{"port": 443}]},
    },
]

CONFIGMAPS = [
    {
        "metadata": {"name": "app-config", "namespace": "default", "creationTimestamp": NOW},
        "data": {"LOG_LEVEL": "info", "FEATURE_FLAGS": "a,b", "TIMEOUT": "30s"},
    }
]

NAMESPACES = [
    {"metadata": {"name": "default", "creationTimestamp": "2026-01-01T00:00:00Z"}, "status": {"phase": "Active"}},
    {"metadata": {"name": "kube-system", "creationTimestamp": "2026-01-01T00:00:00Z"}, "status": {"phase": "Active"}},
    {"metadata": {"name": "wego", "creationTimestamp": "2026-01-01T00:00:00Z"}, "status": {"phase": "Active"}},
]

NODES = [
    {
        "metadata": {"name": "node-a", "labels": {"node-role.kubernetes.io/control-plane": ""},
                     "creationTimestamp": "2026-08-01T00:00:00Z"},
        "status": {"conditions": [{"type": "Ready", "status": "True"}], "nodeInfo": {"kubeletVersion": "v1.31.0"}},
    },
    {
        "metadata": {"name": "node-b", "labels": {"node-role.kubernetes.io/worker": ""},
                     "creationTimestamp": "2026-08-02T00:00:00Z"},
        "status": {"conditions": [{"type": "Ready", "status": "True"}], "nodeInfo": {"kubeletVersion": "v1.31.0"}},
    },
]

EVENTS = [
    {
        "metadata": {"name": "api.1", "namespace": "default", "creationTimestamp": "2026-10-01T09:00:00Z"},
        "involvedObject": {"name": "api-6c4b7d9f8-fghij", "kind": "Pod"},
        "type": "Warning", "reason": "BackOff", "count": 7,
        "message": "Back-off restarting failed container api",
    }
]

STORE = {
    "pods": (True, PODS),
    "deployments": (True, DEPLOYMENTS),
    "services": (True, SERVICES),
    "configmaps": (True, CONFIGMAPS),
    "events": (True, EVENTS),
    "namespaces": (False, NAMESPACES),
    "nodes": (False, NODES),
}
EMPTY_PLURALS = {
    "statefulsets", "daemonsets", "replicasets", "jobs", "cronjobs", "ingresses",
    "secrets", "serviceaccounts", "persistentvolumeclaims", "storageclasses",
}


def load_of(resource, namespace):
    namespaced, items = STORE.get(resource, (True, []))
    for item in items:
        if resource == "pods":
            item.setdefault("kind", "Pod")
            item.setdefault("apiVersion", "v1")
    if namespaced and namespace:
        return [item for item in items if item["metadata"].get("namespace") == namespace]
    return list(items)


def find(resource, namespace, name):
    for item in load_of(resource, namespace):
        if item["metadata"]["name"] == name:
            return item
    return None


def split_path(path):
    """Return (group, namespace, resource, tail) for a Kubernetes REST path."""
    parts = path.strip("/").split("/")
    if not parts or parts[0] not in ("api", "apis"):
        return None
    group = "" if parts[0] == "api" else (parts[1] if len(parts) > 1 else "")
    rest = parts[2:] if parts[0] == "api" else parts[3:]
    namespace = ""
    if rest[:1] == ["namespaces"] and len(rest) >= 3:
        namespace, rest = rest[1], rest[2:]
    if not rest:
        return None
    return group, namespace, rest[0], rest[1:]


def authorized(token, group, resource, verb, namespace):
    """Minimal RBAC evaluator mirroring the restricted ServiceAccount."""
    if token == ADMIN_TOKEN:
        return True
    if token != RESTRICTED_TOKEN:
        return False
    if group == "" and resource == "namespaces":
        return verb in ("get", "list", "watch")
    if namespace != RESTRICTED_NAMESPACE:
        # No cross-namespace access at all.
        return False
    for rule in RESTRICTED_RULES:
        if group not in rule["apiGroups"] and "*" not in rule["apiGroups"]:
            continue
        if not any(item == "*" or item == resource or item == resource + "/*"
                   for item in rule["resources"]):
            continue
        if verb in rule["verbs"] or "*" in rule["verbs"]:
            return True
    return False


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    # -- helpers ---------------------------------------------------------
    def _token(self):
        header = self.headers.get("Authorization") or ""
        return header[7:] if header.lower().startswith("bearer ") else (ADMIN_TOKEN if os.getenv("KC_MOCK_ANONYMOUS") == "1" else "")

    def _send(self, payload, status=200, ctype="application/json"):
        body = payload.encode() if isinstance(payload, str) else json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _body(self):
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b"{}"

    def _not_found(self, path):
        return self._send({"message": f"the server could not find the requested resource: {path}",
                           "reason": "NotFound"}, status=404)

    def _forbidden(self, verb, group, resource, namespace):
        who = "system:serviceaccount:default:readonly-bot" if self._token() == RESTRICTED_TOKEN else "unknown"
        scope = f"in the namespace {namespace}" if namespace else "at the cluster scope"
        return self._send({
            "kind": "Status", "status": "Failure", "reason": "Forbidden",
            "message": f'{resource}.{group or "core"} is forbidden: User "{who}" cannot {verb} resource '
                       f'"{resource}" {scope}',
        }, status=403)

    # -- self subject reviews -------------------------------------------
    def do_POST(self):
        path = urlparse(self.path).path.rstrip("/")
        body = json.loads(self._body() or b"{}")
        token = self._token()

        if path.endswith("/selfsubjectrulesreviews"):
            namespace = (body.get("spec") or {}).get("namespace", "")
            if token == RESTRICTED_TOKEN:
                rules = RESTRICTED_RULES if namespace == RESTRICTED_NAMESPACE else []
            else:
                rules = ADMIN_RULES
            return self._send({"kind": "SelfSubjectRulesReview", "status": {"resourceRules": rules}})

        if path.endswith("/selfsubjectaccessreviews"):
            attributes = (body.get("spec") or {}).get("resourceAttributes") or {}
            namespace = attributes.get("namespace", "")
            allowed = authorized(token, attributes.get("group", ""), attributes.get("resource", ""),
                                 attributes.get("verb", ""), namespace)
            return self._send({
                "kind": "SelfSubjectAccessReview",
                "status": {"allowed": allowed, "reason": "" if allowed else "RBAC: no matching rule"},
            })

        if path.endswith("/selfsubjectreviews"):
            if token == RESTRICTED_TOKEN:
                user = {"username": "system:serviceaccount:default:readonly-bot",
                        "groups": ["system:serviceaccounts", "system:serviceaccounts:default",
                                   "system:authenticated"]}
            else:
                user = {"username": "mock-admin", "groups": ["system:masters", "system:authenticated"]}
            return self._send({"kind": "SelfSubjectReview", "status": {"userInfo": user}})

        return self._not_found(path)

    # -- resource verbs --------------------------------------------------
    def do_GET(self):
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/")
        if path == "/version":
            return self._send({"major": "1", "minor": "31", "gitVersion": "v1.31.0", "platform": "linux/amd64"})

        if path.startswith("/apis/metrics.k8s.io/v1beta1/"):
            token=self._token()
            parts=path.split("/")
            namespace=parts[5] if len(parts)>5 and parts[4]=="namespaces" else None
            name=parts[7] if len(parts)>7 else None
            items=[]
            stamp=time.strftime("%Y-%m-%dT%H:%M:%SZ",time.gmtime())
            for pod in PODS:
                meta=pod["metadata"]
                if namespace and namespace!=meta["namespace"]:continue
                if not authorized(token,"","pods","get",meta["namespace"]):continue
                if name and name!=meta["name"]:continue
                items.append({"metadata":meta,"timestamp":stamp,"window":"20s","containers":[{"name":c["name"],"usage":{"cpu":"125000000n","memory":"128Mi"}} for c in pod["spec"]["containers"]]})
            if name:return self._send(items[0]) if items else self._not_found(path)
            return self._send({"items":items})
        if path.endswith("/proxy/stats/summary"):
            if self._token()==RESTRICTED_TOKEN:return self._forbidden("get","","nodes/proxy",None)
            node=path.split("/")[4]
            stamp=time.strftime("%Y-%m-%dT%H:%M:%SZ",time.gmtime())
            return self._send({"pods":[{"podRef":{**p["metadata"]},"ephemeral-storage":{"time":stamp,"usedBytes":1048576},"network":{"time":stamp,"rxBytes":int(time.time()*2048),"txBytes":int(time.time()*1024)}} for p in PODS if p["spec"]["nodeName"]==node]})

        if path == "/api":
            return self._send({"kind": "APIVersions", "apiVersion": "v1", "versions": ["v1"]})
        if path == "/apis":
            return self._send({"kind": "APIGroupList", "apiVersion": "v1", "groups": []})
        if path == "/api/v1":
            return self._send({"kind": "APIResourceList", "apiVersion": "v1", "groupVersion": "v1", "resources": [
                {"name": "pods", "singularName": "pod", "namespaced": True, "kind": "Pod", "verbs": ["get", "list"], "shortNames": ["po"]},
                {"name": "namespaces", "singularName": "namespace", "namespaced": False, "kind": "Namespace", "verbs": ["get", "list"]}
            ]})
        split = split_path(path)
        if split is None:
            return self._not_found(path)
        group, namespace, resource, tail = split
        token = self._token()

        if len(tail) == 2 and tail[1] == "log" and resource == "pods":
            if not authorized(token, "", "pods/log", "get", namespace):
                return self._forbidden("get", "", "pods/log", namespace)
            pod = find("pods", namespace, tail[0])
            if pod is None:
                return self._not_found(path)
            query = parse_qs(parsed.query)
            container = (query.get("container") or ["?"])[0]
            tail_lines = int((query.get("tailLines") or ["500"])[0])
            lines = [f"2026-10-01T10:00:{i:02d}Z [{container}] request handled in {12 + i}ms"
                     for i in range(min(tail_lines, 40))]
            if container == "api":
                lines.insert(0, "2026-10-01T09:59:58Z [api] FATAL refusing to start: missing DATABASE_URL")
            if (query.get("follow") or [""])[0] == "true":
                self.send_response(200)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True
                try:
                    self.wfile.write(("\n".join(lines) + "\n").encode("utf-8"))
                    self.wfile.flush()
                    for i in range(12):
                        time.sleep(0.12)
                        self.wfile.write(f"\x1b[32m实时中文日志 {i} ✓\x1b[0m\n".encode("utf-8"))
                        self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass
                return
            return self._send("\n".join(lines) + "\n", ctype="text/plain; charset=utf-8")

        if tail:
            if not authorized(token, group, resource, "get", namespace):
                return self._forbidden("get", group, resource, namespace)
            item = find(resource, namespace, tail[0])
            if item is None:
                return self._not_found(path)
            return self._send(item)

        if not authorized(token, group, resource, "list", namespace):
            return self._forbidden("list", group, resource, namespace)
        return self._send({"kind": "List", "apiVersion": "v1", "metadata": {}, "items": load_of(resource, namespace)})

    def do_PATCH(self):
        path = urlparse(self.path).path.rstrip("/")
        patch = json.loads(self._body() or b"{}")
        split = split_path(path)
        if split is None:
            return self._not_found(path)
        group, namespace, resource, tail = split
        token = self._token()
        if not tail:
            return self._not_found(path)
        if not authorized(token, group, resource, "patch", namespace):
            return self._forbidden("patch", group, resource, namespace)
        with LOCK:
            item = find(resource, namespace, tail[0])
            if item is None:
                return self._not_found(path)
            if tail[-1] == "scale":
                replicas = patch.get("spec", {}).get("replicas")
                if isinstance(replicas, int):
                    item["spec"]["replicas"] = replicas
                    item["status"]["replicas"] = replicas
                    item["status"]["readyReplicas"] = replicas
            else:
                template = patch.get("spec", {}).get("template", {}) or {}
                annotations = (template.get("metadata", {}) or {}).get("annotations")
                if annotations:
                    item["metadata"].setdefault("annotations", {}).update(annotations)
        return self._send(item)

    def do_DELETE(self):
        path = urlparse(self.path).path.rstrip("/")
        self._body()
        split = split_path(path)
        if split is None:
            return self._not_found(path)
        group, namespace, resource, tail = split
        token = self._token()
        if not tail:
            return self._not_found(path)
        if not authorized(token, group, resource, "delete", namespace):
            return self._forbidden("delete", group, resource, namespace)
        with LOCK:
            _, items = STORE.get(resource, (True, []))
            before = len(items)
            items[:] = [item for item in items if item["metadata"]["name"] != tail[0]]
            if len(items) == before:
                return self._not_found(path)
        return self._send({"kind": "Status", "apiVersion": "v1", "status": "Success"})


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18080
    print(f"mock Kubernetes API listening on http://127.0.0.1:{port}", flush=True)
    print(f"  admin token      : {ADMIN_TOKEN}", flush=True)
    print(f"  restricted token : {RESTRICTED_TOKEN} (namespace {RESTRICTED_NAMESPACE})", flush=True)
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
