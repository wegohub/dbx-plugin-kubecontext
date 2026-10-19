package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

func TestResourceMutationMethodsArePermanentlyRejected(t *testing.T) {
	p := newPlugin()
	// Legacy configurations and arbitrary resource kinds cannot enable mutations.
	for _, method := range []string{"kube/delete", "kube/scale", "kube/restart"} {
		for kind := range kindCatalog {
			payload, _ := json.Marshal(map[string]any{"connectionId": "legacy", "context": "prod", "kind": kind, "name": "example", "replicas": 0, "read_only": false})
			if _, err := p.Handle(dbx.RequestContext{}, method, payload, nil); err == nil || err.Code != -32013 {
				t.Fatalf("%s %s was not blocked before resolving a connection: %v", method, kind, err)
			}
		}
	}
}

func TestReadOnlyTransportBlocksWritesBeforeSendingHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client, err := buildClient(&settings{AuthMode: "token", Server: server.URL, Token: "test", Timeout: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodDelete, http.MethodPatch, http.MethodPut, http.MethodPost} {
		if _, err := client.request(context.Background(), method, "/api/v1/namespaces/prod/pods/example", nil, "application/json"); err == nil {
			t.Fatalf("allowed %s", method)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("a resource write reached the server")
	}
	if _, err := client.request(context.Background(), http.MethodGet, "/version", nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/apis/authentication.k8s.io/v1/selfsubjectreviews", "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"} {
		if _, err := client.request(context.Background(), http.MethodPost, path, []byte(`{}`), "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 4 {
		t.Fatal(requests.Load())
	}
}

func TestRichPodSummaryIncludesActualFailureAndScheduling(t *testing.T) {
	var object map[string]any
	if err := json.Unmarshal([]byte(`{
      "metadata":{"name":"api-1","namespace":"prod","creationTimestamp":"2026-10-01T10:00:00Z","ownerReferences":[{"kind":"ConfigMap","name":"helper"},{"kind":"ReplicaSet","name":"api-123","controller":true}]},
      "spec":{"nodeName":"node-a","serviceAccountName":"app","containers":[{"name":"app","image":"app:v2"},{"name":"proxy","image":"proxy:v1"}]},
      "status":{"phase":"Running","podIP":"10.0.0.8","hostIP":"192.168.1.2","qosClass":"Burstable","containerStatuses":[{"name":"app","restartCount":3,"ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off restarting failed container"}}}],"initContainerStatuses":[{"restartCount":2,"state":{"terminated":{"exitCode":0}}}]}
    }`), &object); err != nil {
		t.Fatal(err)
	}
	row := summarize("pods", object)
	if row.Status != "CrashLoopBackOff" || row.Ready != "0/2" {
		t.Fatalf("status=%s ready=%s", row.Status, row.Ready)
	}
	for key, want := range map[string]string{"restarts": "5", "node": "node-a", "podIP": "10.0.0.8", "images": "app:v2, proxy:v1", "owner": "ReplicaSet/api-123", "serviceAccount": "app", "qos": "Burstable", "reason": "back-off restarting failed container"} {
		if row.Fields[key] != want {
			t.Fatalf("%s=%q, want %q", key, row.Fields[key], want)
		}
	}
	if row.CreatedAt != "2026-10-01T10:00:00Z" {
		t.Fatal(row.CreatedAt)
	}
	object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-19T10:00:00Z"
	if summarize("pods", object).Status != "Terminating" {
		t.Fatal("terminating pod still shown as running")
	}
}

func TestRichServiceAndWorkloadSummaries(t *testing.T) {
	var service, deployment map[string]any
	json.Unmarshal([]byte(`{"spec":{"type":"NodePort","clusterIP":"10.96.0.8","ports":[{"port":80,"nodePort":30080,"protocol":"TCP"}],"selector":{"app":"api"}},"status":{"loadBalancer":{"ingress":[{"ip":"192.0.2.1"}]}}}`), &service)
	row := summarize("services", service)
	if row.Fields["ports"] != "80:30080/TCP" || row.Fields["clusterIP"] != "10.96.0.8" || row.Fields["externalIPs"] != "192.0.2.1" || row.Fields["selector"] != "app=api" {
		t.Fatal(row.Fields)
	}
	json.Unmarshal([]byte(`{"spec":{"replicas":3,"selector":{"matchLabels":{"tier":"api","app":"web"}},"template":{"spec":{"containers":[{"image":"app:v3"},{"image":"proxy:v2"}]}}},"status":{"readyReplicas":2,"updatedReplicas":3,"availableReplicas":2}}`), &deployment)
	row = summarize("deployments", deployment)
	if row.Ready != "2/3" || row.Fields["images"] != "app:v3, proxy:v2" || row.Fields["selector"] != "app=web, tier=api" || row.Fields["updated"] != "3" {
		t.Fatal(row)
	}
}
