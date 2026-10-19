package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAggregateMergeAndTerminalIsolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	base := &settings{ConnectionID: "c", AuthMode: "kubeconfig", KubeconfigPath: path}
	p := newPlugin()
	original := `apiVersion: v1
kind: Config
current-context: alpha
clusters:
- name: local
  cluster:
    server: http://127.0.0.1:1
    certificate-authority: certs/ca.pem
contexts:
- name: alpha
  context: {cluster: local, user: local}
users:
- name: local
  user: {token: private-token}
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	incoming := `apiVersion: v1
kind: Config
current-context: beta
clusters:
- name: second
  cluster: {server: 'http://127.0.0.1:2'}
contexts:
- name: beta
  context: {cluster: second, user: second}
users:
- name: second
  user: {token: second-token}
`
	p.rememberConfig("c", base, nil)
	other := *base
	other.ConnectionID = "c2"
	p.rememberConfig("c2", &other, nil)
	p.sessions[sessionKey("c", "alpha")] = &session{}
	p.sessions[sessionKey("c2", "alpha")] = &session{}
	result, perr := p.mergeInto(base, incoming, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if len(p.sessions) != 0 {
		t.Fatal("stale credentials cached after merge")
	}
	if len(result["contexts"].([]contextInfo)) != 2 {
		t.Fatal(result)
	}
	doc, err := readKubeconfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc["current-context"] != "alpha" {
		t.Fatal("merge changed current context")
	}
	backups, _ := filepath.Glob(path + ".bak-*")
	if len(backups) != 1 {
		t.Fatal(backups)
	}
	raw, _ := os.ReadFile(backups[0])
	if string(raw) != original {
		t.Fatal("backup not byte-identical")
	}
	snapshot, cleanup, err := p.terminalConfig(base, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := writeCurrentContext(base, "beta"); err != nil {
		t.Fatal(err)
	}
	isolated, err := readKubeconfigFile(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if isolated["current-context"] != "alpha" {
		t.Fatal("terminal followed global context")
	}
	cluster := asMap(asObjectList(isolated["clusters"])[0]["cluster"])
	if cluster["certificate-authority"] != filepath.Join(dir, "certs/ca.pem") {
		t.Fatal(cluster)
	}
	info, _ := os.Stat(snapshot)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	if _, _, err := p.terminalConfig(base, "missing"); err == nil {
		t.Fatal("unknown context accepted")
	}
}

func TestStripANSIWithUnicode(t *testing.T) {
	cases := map[string]string{
		"\x1b[31m中文日志\x1b[0m":                             "中文日志",
		"\x1b]8;;https://example.com\x07链接\x1b]8;;\x1b\\": "链接",
		"\x1b[2K\x1b[1Gready":                             "ready",
		"plain UTF-8 日志":                                  "plain UTF-8 日志",
	}
	for input, want := range cases {
		if got := stripANSI(input); got != want {
			t.Fatalf("%q != %q", got, want)
		}
	}
}
func TestRejectedImportDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	p := newPlugin()
	base := &settings{KubeconfigPath: path}
	for _, bad := range []string{"", "invalid: [", "contexts: []", strings.Repeat("x", maxKubeconfigBytes+1)} {
		if _, perr := p.mergeInto(base, bad, ""); perr == nil {
			t.Fatal("accepted invalid import")
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid import wrote target")
	}
}
