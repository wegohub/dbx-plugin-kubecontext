package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func objectJSON(t *testing.T, value string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		t.Fatal(err)
	}
	return object
}
func assertNumber(t *testing.T, value *float64, want float64) {
	t.Helper()
	if value == nil || math.Abs(*value-want) > 1e-6 {
		t.Fatalf("%v != %v", value, want)
	}
}
func TestKubernetesQuantitiesAndEffectiveBudgets(t *testing.T) {
	for raw, want := range map[string]float64{"125m": .125, "12688658n": .012688658, "23028Ki": 23580672, "1.5Gi": 1610612736, "1e3": 1000, "0": 0, "10M": 1e7, ".5": .5} {
		assertNumber(t, quantity(raw), want)
	}
	for _, raw := range []string{"", "-1", "NaN", "1bad", "1e999"} {
		if quantity(raw) != nil {
			t.Fatal(raw)
		}
	}
	pod := objectJSON(t, `{"spec":{"containers":[{"name":"app","resources":{"requests":{"cpu":"100m","memory":"128Mi"},"limits":{"cpu":"1","memory":"512Mi"}}},{"name":"helper","resources":{"requests":{"cpu":"50m"}}}],"initContainers":[{"name":"sidecar","restartPolicy":"Always","resources":{"requests":{"cpu":"20m"}}},{"name":"init","resources":{"requests":{"cpu":"300m"}}}],"overhead":{"cpu":"10m"}}}`)
	b := resourceBudget(pod)
	assertNumber(t, b.CPURequest, 330)
	assertNumber(t, b.MemoryRequest, 128<<20)
	if b.CPULimit != nil || !b.CPULimitPartial || b.MemoryLimit != nil || !b.MemoryLimitPartial {
		t.Fatal(b)
	}
	if len(b.Containers) != 4 || b.Containers[2].Kind != "sidecar" || b.Containers[3].Kind != "init" {
		t.Fatal(b)
	}
	pod["spec"].(map[string]any)["resources"] = map[string]any{"limits": map[string]any{"cpu": "2"}}
	assertNumber(t, resourceBudget(pod).CPULimit, 2010)
	zero := resourceBudget(objectJSON(t, `{"spec":{"containers":[{"resources":{"requests":{"cpu":"0"},"limits":{"cpu":"0"}}}]}}`))
	assertNumber(t, zero.CPURequest, 0)
	assertNumber(t, zero.CPULimit, 0)
}
func metricPlugin(t *testing.T, handler http.HandlerFunc) (*plugin, map[string]any) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p := newPlugin()
	p.rememberConfig("metrics", &settings{ConnectionID: "metrics", AuthMode: "token", Server: server.URL, Token: "test", Namespace: "prod", Timeout: time.Second}, nil)
	return p, map[string]any{"connectionId": "metrics", "context": "token", "namespace": "prod", "name": "api"}
}
func TestPodMetricsPartialAndSummaryFallback(t *testing.T) {
	for _, mode := range []string{"complete", "summary-denied", "metrics-denied", "both-denied", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			p, params := metricPlugin(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("monitor sent %s", r.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/version":
					w.Write([]byte(`{"gitVersion":"v1.36"}`))
				case r.URL.Path == "/api/v1/namespaces/prod/pods/api":
					w.Write([]byte(`{"metadata":{"uid":"uid-new"},"spec":{"nodeName":"node-a","containers":[{"name":"app"}]}}`))
				case strings.Contains(r.URL.Path, "metrics.k8s.io"):
					if mode == "metrics-denied" || mode == "both-denied" {
						w.WriteHeader(403)
						w.Write([]byte(`{"message":"metrics forbidden"}`))
						return
					}
					w.Write([]byte(`{"timestamp":"2026-10-19T12:00:00Z","window":"20s","containers":[{"name":"app","usage":{"cpu":"125000000n","memory":"128Mi"}},{"name":"proxy","usage":{"cpu":"25m","memory":"16Mi"}}]}`))
				case strings.HasSuffix(r.URL.Path, "/stats/summary"):
					if mode == "summary-denied" || mode == "both-denied" {
						w.WriteHeader(403)
						w.Write([]byte(`{"message":"nodes/proxy forbidden"}`))
						return
					}
					uid := "uid-new"
					if mode == "replacement" {
						uid = "uid-old"
					}
					w.Write([]byte(`{"pods":[{"podRef":{"uid":"` + uid + `","name":"api","namespace":"prod"},"cpu":{"usageNanoCores":200000000,"time":"2026-10-19T12:00:00Z"},"memory":{"workingSetBytes":1234},"ephemeral-storage":{"usedBytes":2048},"network":{"time":"2026-10-19T12:00:00Z","rxBytes":1024,"txBytes":2048}}]}`))
				default:
					t.Errorf("unexpected %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			raw, err := p.handlePodMetrics(params)
			if err != nil {
				t.Fatal(err)
			}
			result := raw.(map[string]any)
			if mode == "both-denied" {
				if result["cpuMillicores"] != nil || result["network"] != nil {
					t.Fatal(result)
				}
				return
			}
			assertNumber(t, result["cpuMillicores"].(*float64), map[bool]float64{true: 200, false: 150}[mode == "metrics-denied"])
			if mode == "summary-denied" || mode == "replacement" {
				if result["diskUsedBytes"] != nil || result["network"] != nil {
					t.Fatal(result)
				}
				if len(result["warnings"].([]map[string]string)) == 0 {
					t.Fatal("missing degradation reason")
				}
			} else {
				assertNumber(t, result["diskUsedBytes"].(*float64), 2048)
				assertNumber(t, result["network"].(map[string]any)["rxBytes"].(*float64), 1024)
			}
		})
	}
}
func TestContainerTerminalUsesValidatedArgv(t *testing.T) {
	p, params := metricPlugin(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			w.Write([]byte(`{}`))
			return
		}
		w.Write([]byte(`{"status":{"containerStatuses":[{"name":"app","state":{"running":{}}}]}}`))
	})
	params["pod"] = "api"
	params["container"] = "app"
	params["shell"] = "/bin/sh"
	cmd, err := p.terminalCommand(params, "/tmp/path with spaces/config")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"kubectl", "--kubeconfig", "/tmp/path with spaces/config", "--namespace", "prod", "exec", "-it", "api", "-c", "app", "--", "/bin/sh"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatal(cmd.Args)
	}
	for _, shell := range []string{"sh -c rm", "/bin/sh; echo bad", "/usr/bin/python"} {
		params["shell"] = shell
		if _, err := p.terminalCommand(params, "config"); err == nil {
			t.Fatal(shell)
		}
	}
	params["shell"] = "/bin/bash"
	params["container"] = "stopped"
	if _, err := p.terminalCommand(params, "config"); err == nil {
		t.Fatal("entered a stopped container")
	}
}
