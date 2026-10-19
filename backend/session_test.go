package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSelectedContextOwnsNamespace(t *testing.T) {
	podPaths := make(chan string, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/version":
			fmt.Fprint(w, `{"gitVersion":"v1.31.0"}`)
		case r.URL.Path == "/api/v1/namespaces":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"reason":"Forbidden","message":"namespace enumeration denied"}`)
		case strings.HasSuffix(r.URL.Path, "/pods"):
			podPaths <- r.URL.Path
			fmt.Fprint(w, `{"items":[]}`)
		default:
			fmt.Fprint(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "config")
	config := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: ht-wego
clusters:
- name: cluster
  cluster: {server: %q}
users:
- name: user
  user: {token: test-token}
contexts:
- name: hkg-prod-backendhub
  context: {cluster: cluster, user: user, namespace: backendhub}
- name: hkg-prod-scanx
  context: {cluster: cluster, user: user, namespace: scanx}
- name: hkg-test
  context: {cluster: cluster, user: user, namespace: backendhub}
- name: ht-wego
  context: {cluster: cluster, user: user, namespace: wego}
- name: no-namespace
  context: {cluster: cluster, user: user}
`, server.URL)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	p := newPlugin()
	base := &settings{ConnectionID: "c", AuthMode: "kubeconfig", KubeconfigPath: path, Namespace: "wego", Timeout: time.Second}
	p.rememberConfig("c", base, nil)
	cases := []struct{ name, namespace string }{
		{"ht-wego", "wego"}, {"hkg-prod-backendhub", "backendhub"},
		{"hkg-prod-scanx", "scanx"}, {"hkg-test", "backendhub"}, {"no-namespace", "default"},
		{"ht-wego", "wego"}, // Revisit a cached session after switching several times.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"connectionId": "c", "context": tc.name, "kind": "pods"}
			result, perr := p.handleOverview(params)
			if perr != nil {
				t.Fatal(perr)
			}
			overview := result.(map[string]any)
			if overview["namespace"] != tc.namespace {
				t.Fatalf("namespace = %v, want %s", overview["namespace"], tc.namespace)
			}
			// Even without permission to enumerate namespaces, the selected context remains available.
			names := overview["namespaces"].([]string)
			if len(names) != 1 || names[0] != tc.namespace {
				t.Fatalf("namespace options: %v", names)
			}
			if _, perr := p.handleList(params); perr != nil {
				t.Fatal(perr)
			}
			if got, want := <-podPaths, "/api/v1/namespaces/"+tc.namespace+"/pods"; got != want {
				t.Fatalf("GET %s, want %s", got, want)
			}
		})
	}
	if base.Namespace != "wego" {
		t.Fatal("session mutated connection settings")
	}
	// Explicit UI selection still overrides the context default for an individual request.
	if _, perr := p.handleList(map[string]any{"connectionId": "c", "context": "hkg-test", "kind": "pods", "namespace": "chosen"}); perr != nil {
		t.Fatal(perr)
	}
	if got := <-podPaths; got != "/api/v1/namespaces/chosen/pods" {
		t.Fatal(got)
	}
	// API Server + Token connections keep their own configured namespace.
	p.rememberConfig("token", &settings{ConnectionID: "token", AuthMode: "token", Server: server.URL, Token: "test-token", Namespace: "token-ns", Timeout: time.Second}, nil)
	if _, perr := p.handleList(map[string]any{"connectionId": "token", "context": "token", "kind": "pods"}); perr != nil {
		t.Fatal(perr)
	}
	if got := <-podPaths; got != "/api/v1/namespaces/token-ns/pods" {
		t.Fatal(got)
	}
}
