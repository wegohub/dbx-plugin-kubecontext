package main

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeoutSecs = 15
	minTimeoutSecs     = 5
	maxTimeoutSecs     = 120
)

// settings is the resolved view of one saved DBX connection.
//
// DBX hands the backend the *hydrated* connection (secrets included) on
// connection/test, connection/connect and connection/action. The workbench UI
// only ever sees the connection id, so everything secret lives here.
type settings struct {
	ConnectionID string
	Name         string
	AuthMode     string

	KubeconfigPath string
	Kubeconfig     string
	Context        string

	Server string
	Port   int
	Token  string
	CA     string

	Namespace string
	Insecure  bool
	AllowExec bool
	ReadOnly  bool
	Timeout   time.Duration
}

func asMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

func stringValue(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func intValue(m map[string]any, key string, fallback int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return fallback
}

func boolValue(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true")
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func settingsFromConnection(connection map[string]any) (*settings, error) {
	if connection == nil {
		return nil, errors.New("missing connection payload")
	}
	id := stringValue(connection, "id")
	if id == "" {
		return nil, errors.New("connection payload has no id")
	}
	config := asMap(connection["external_config"])
	secrets := asMap(connection["connection_secrets"])

	timeout := intValue(config, "connect_timeout_secs", defaultTimeoutSecs)
	if timeout < minTimeoutSecs {
		timeout = minTimeoutSecs
	}
	if timeout > maxTimeoutSecs {
		timeout = maxTimeoutSecs
	}

	s := &settings{
		ConnectionID: id,
		Name:         firstNonEmpty(stringValue(connection, "name"), "Kubernetes cluster"),
		AuthMode:     firstNonEmpty(stringValue(config, "auth_mode"), "kubeconfig"),

		KubeconfigPath: expandHome(stringValue(config, "kubeconfig_path")),
		Kubeconfig:     strings.TrimSpace(stringValue(secrets, "kubeconfig")),
		Context:        stringValue(config, "context"),

		Server: stringValue(connection, "host"),
		Port:   intValue(connection, "port", 0),
		Token:  stringValue(secrets, "token"),
		CA:     stringValue(secrets, "ca_cert"),

		Namespace: firstNonEmpty(stringValue(config, "namespace"), "default"),
		Insecure:  boolValue(config, "insecure_skip_tls_verify"),
		AllowExec: boolValue(config, "allow_exec"),
		ReadOnly:  true,
		Timeout:   time.Duration(timeout) * time.Second,
	}
	if s.AuthMode == "kubeconfig" && s.KubeconfigPath == "" && s.Kubeconfig == "" {
		s.KubeconfigPath, _ = targetPath(s)
	}
	if s.AuthMode == "token" && s.Server == "" {
		return nil, errors.New("API Server 地址为空：请填写 server，或改用 kubeconfig 认证")
	}
	return s, nil
}

func expandHome(path string) string {
	if path == "" || !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := osUserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return home + path[1:]
	}
	return path
}
