package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 32 << 20

// kubeClient is a deliberately small Kubernetes REST client: list/get/patch/
// delete plus pod logs. Swap in client-go if you need discovery, informers,
// exec/attach streaming or apply strategies.
type kubeClient struct {
	server    string
	token     string
	tokenFile string
	http      *http.Client
	timeout   time.Duration
}

type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Kubernetes API HTTP %d: %s", e.Status, e.Message)
	}
	if e.Reason != "" {
		return fmt.Sprintf("Kubernetes API HTTP %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("Kubernetes API HTTP %d", e.Status)
}

func buildClient(s *settings, runtime map[string]any) (*kubeClient, error) {
	var creds *credentials
	var err error
	if s.AuthMode == "token" {
		creds = &credentials{
			ServerURL: s.Server,
			Token:     s.Token,
			CAPEM:     []byte(s.CA),
			Insecure:  s.Insecure,
			Context:   "token",
		}
		if s.Port > 0 {
			creds.ServerURL = withPort(creds.ServerURL, s.Port)
		}
	} else {
		creds, err = resolveKubeconfig(s)
		if err != nil {
			return nil, err
		}
		if s.Context == "" {
			s.Context = creds.Context
		}
	}

	server, serverName, err := normalizeServer(creds.ServerURL, creds.ServerName)
	if err != nil {
		return nil, err
	}

	// DBX resolves SSH/proxy transport layers before it hands the connection to
	// the plugin. When it provides a runtime endpoint, dial that instead of the
	// address recorded in the kubeconfig, but keep verifying against the
	// original server name.
	if host := stringValue(runtime, "host"); host != "" {
		if port := intValue(runtime, "port", 0); port > 0 {
			parsed, parseErr := url.Parse(server)
			if parseErr != nil {
				return nil, fmt.Errorf("API Server 地址无效：%w", parseErr)
			}
			if serverName == "" {
				serverName = parsed.Hostname()
			}
			parsed.Host = net.JoinHostPort(host, strconv.Itoa(port))
			server = parsed.String()
		}
	}

	tlsConfig, err := buildTLSConfig(creds, serverName)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   s.Timeout,
		ResponseHeaderTimeout: s.Timeout,
	}
	return &kubeClient{
		server:    strings.TrimRight(server, "/"),
		token:     creds.Token,
		tokenFile: creds.TokenFile,
		timeout:   s.Timeout,
		http:      &http.Client{Transport: transport, Timeout: s.Timeout * 2},
	}, nil
}

func buildTLSConfig(creds *credentials, serverName string) (*tls.Config, error) {
	config := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: creds.Insecure,
		ServerName:         serverName,
	}
	if !creds.Insecure && len(creds.CAPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(creds.CAPEM) {
			return nil, errors.New("CA 证书无法解析：请确认为 PEM 格式")
		}
		config.RootCAs = pool
	}
	if len(creds.CertPEM) > 0 && len(creds.KeyPEM) > 0 {
		pair, err := tls.X509KeyPair(creds.CertPEM, creds.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("客户端证书/私钥不匹配：%w", err)
		}
		config.Certificates = []tls.Certificate{pair}
	}
	return config, nil
}

// normalizeServer makes the URL absolute and returns its hostname for TLS SNI.
func normalizeServer(raw, serverName string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("API Server 地址为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("API Server 地址无效：%w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", "", fmt.Errorf("API Server 只支持 http/https：%s", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", "", errors.New("API Server 缺少主机名")
	}
	if serverName == "" {
		serverName = parsed.Hostname()
	}
	return parsed.String(), serverName, nil
}

func withPort(raw string, port int) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if parsed.Port() != "" {
		return parsed.String()
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), strconv.Itoa(port))
	return parsed.String()
}

func (c *kubeClient) bearerToken() string {
	if c.token != "" {
		return c.token
	}
	if c.tokenFile != "" {
		if raw, err := readFileString(c.tokenFile); err == nil {
			return strings.TrimSpace(raw)
		}
	}
	return ""
}

func (c *kubeClient) request(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, error) {
	// Resource access is permanently read-only. POST is restricted to the
	// non-persistent self-review endpoints needed to inspect RBAC/identity.
	review := path == "/apis/authentication.k8s.io/v1/selfsubjectreviews" ||
		path == "/apis/authorization.k8s.io/v1/selfsubjectrulesreviews" ||
		path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews"
	if method != http.MethodGet && !(method == http.MethodPost && review) {
		return nil, errors.New("插件为资源只读模式，禁止修改或删除集群资源")
	}

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token := c.bearerToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Kubernetes API 失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("读取 Kubernetes API 响应失败：%w", err)
	}
	if resp.StatusCode >= 300 {
		apiErr := &apiError{Status: resp.StatusCode}
		var status struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(data, &status) == nil {
			apiErr.Message = status.Message
			apiErr.Reason = status.Reason
		}
		return nil, apiErr
	}
	return data, nil
}

func (c *kubeClient) getJSON(ctx context.Context, path string, out any) error {
	data, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *kubeClient) getText(ctx context.Context, path string) (string, error) {
	data, err := c.request(ctx, http.MethodGet, path, nil, "")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// openStream issues a request and hands back the live body, for follow=true log
// streaming and any other long-lived response. No client timeout applies.
func (c *kubeClient) openStream(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token := c.bearerToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	transport, _ := c.http.Transport.(*http.Transport)
	if transport != nil {
		// A response-header timeout would abort a long follow stream.
		clone := transport.Clone()
		clone.ResponseHeaderTimeout = 0
		transport = clone
	} else {
		transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	}
	streaming := &http.Client{Transport: transport}
	resp, err := streaming.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Kubernetes API 失败：%w", err)
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		apiErr := &apiError{Status: resp.StatusCode}
		var status struct {
			Message string `json:"message"`
			Reason  string `json:"reason"`
		}
		if json.Unmarshal(data, &status) == nil {
			apiErr.Message = status.Message
			apiErr.Reason = status.Reason
		}
		return nil, apiErr
	}
	return resp, nil
}

// serverVersion performs the connection/test probe.
func (c *kubeClient) serverVersion(ctx context.Context) (map[string]string, error) {
	var version struct {
		Major      string `json:"major"`
		Minor      string `json:"minor"`
		GitVersion string `json:"gitVersion"`
		Platform   string `json:"platform"`
	}
	if err := c.getJSON(ctx, "/version", &version); err != nil {
		return nil, err
	}
	return map[string]string{
		"gitVersion": version.GitVersion,
		"major":      version.Major,
		"minor":      version.Minor,
		"platform":   version.Platform,
	}, nil
}
