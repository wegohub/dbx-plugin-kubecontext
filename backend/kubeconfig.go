package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// credentials is the credential material one kube context resolves to.
type credentials struct {
	ServerURL  string
	Token      string
	TokenFile  string
	CAPEM      []byte
	CertPEM    []byte
	KeyPEM     []byte
	Insecure   bool
	ServerName string
	Context    string
	AuthInfo   string
}

type kubeConfigFile struct {
	CurrentContext string         `yaml:"current-context"`
	Clusters       []namedCluster `yaml:"clusters"`
	Contexts       []namedContext `yaml:"contexts"`
	Users          []namedUser    `yaml:"users"`
}

type namedCluster struct {
	Name    string       `yaml:"name"`
	Cluster clusterEntry `yaml:"cluster"`
}

type clusterEntry struct {
	Server                   string `yaml:"server"`
	TLSServerName            string `yaml:"tls-server-name"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

type namedContext struct {
	Name    string       `yaml:"name"`
	Context contextEntry `yaml:"context"`
}

type contextEntry struct {
	Cluster   string `yaml:"cluster"`
	User      string `yaml:"user"`
	Namespace string `yaml:"namespace"`
}

type namedUser struct {
	Name string    `yaml:"name"`
	User userEntry `yaml:"user"`
}

type userEntry struct {
	Token                 string      `yaml:"token"`
	TokenFile             string      `yaml:"tokenFile"`
	ClientCertificate     string      `yaml:"client-certificate"`
	ClientCertificateData string      `yaml:"client-certificate-data"`
	ClientKey             string      `yaml:"client-key"`
	ClientKeyData         string      `yaml:"client-key-data"`
	Exec                  *execConfig `yaml:"exec"`
}

type execConfig struct {
	APIVersion      string   `yaml:"apiVersion"`
	Command         string   `yaml:"command"`
	Args            []string `yaml:"args"`
	InteractiveMode string   `yaml:"interactiveMode"`
	Env             []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
}

type execCredential struct {
	Status struct {
		Token                 string `json:"token"`
		ClientCertificateData string `json:"clientCertificateData"`
		ClientKeyData         string `json:"clientKeyData"`
	} `json:"status"`
}

func osUserHomeDir() (string, error) {
	return os.UserHomeDir()
}

// readKubeconfig returns the raw kubeconfig plus the directory relative file
// references (certificate-authority, client-key, ...) resolve against.
func readKubeconfig(s *settings) ([]byte, string, error) {
	if s.KubeconfigPath != "" {
		info, err := os.Stat(s.KubeconfigPath)
		if err == nil && !info.IsDir() {
			raw, readErr := os.ReadFile(s.KubeconfigPath)
			if readErr != nil {
				return nil, "", fmt.Errorf("读取 kubeconfig 失败：%w", readErr)
			}
			return raw, filepath.Dir(s.KubeconfigPath), nil
		}
		if s.Kubeconfig == "" {
			return nil, "", fmt.Errorf("kubeconfig 文件不存在：%s", s.KubeconfigPath)
		}
	}
	if s.Kubeconfig != "" {
		base := ""
		if s.KubeconfigPath != "" {
			base = filepath.Dir(s.KubeconfigPath)
		}
		if base == "" {
			if wd, err := os.Getwd(); err == nil {
				base = wd
			}
		}
		return []byte(s.Kubeconfig), base, nil
	}
	return nil, "", errors.New("请提供 kubeconfig 文件路径，或把 kubeconfig 内容作为密钥上传")
}

func parseKubeconfig(raw []byte) (*kubeConfigFile, error) {
	var cfg kubeConfigFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("kubeconfig 解析失败：%w", err)
	}
	if len(cfg.Contexts) == 0 {
		return nil, errors.New("kubeconfig 里没有任何 context")
	}
	return &cfg, nil
}

func (cfg *kubeConfigFile) contextNames() []string {
	names := make([]string, 0, len(cfg.Contexts))
	for _, item := range cfg.Contexts {
		names = append(names, item.Name)
	}
	return names
}

func (cfg *kubeConfigFile) findContext(name string) *contextEntry {
	for i := range cfg.Contexts {
		if cfg.Contexts[i].Name == name {
			return &cfg.Contexts[i].Context
		}
	}
	return nil
}

func (cfg *kubeConfigFile) findCluster(name string) *clusterEntry {
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == name {
			return &cfg.Clusters[i].Cluster
		}
	}
	return nil
}

func (cfg *kubeConfigFile) findUser(name string) *userEntry {
	if name == "" {
		return nil
	}
	for i := range cfg.Users {
		if cfg.Users[i].Name == name {
			return &cfg.Users[i].User
		}
	}
	return nil
}

// resolveKubeconfig turns a settings.auth_mode = kubeconfig connection into
// transport credentials for exactly one context.
func resolveKubeconfig(s *settings) (*credentials, error) {
	raw, baseDir, err := readKubeconfig(s)
	if err != nil {
		return nil, err
	}
	cfg, err := parseKubeconfig(raw)
	if err != nil {
		return nil, err
	}

	contextName := s.Context
	if contextName == "" {
		contextName = cfg.CurrentContext
	}
	if contextName == "" {
		return nil, fmt.Errorf("kubeconfig 未设置 current-context，请显式选择一个：%s", strings.Join(cfg.contextNames(), ", "))
	}
	ctx := cfg.findContext(contextName)
	if ctx == nil {
		return nil, fmt.Errorf("kubeconfig 中找不到 context %q，可选：%s", contextName, strings.Join(cfg.contextNames(), ", "))
	}
	cluster := cfg.findCluster(ctx.Cluster)
	if cluster == nil {
		return nil, fmt.Errorf("context %q 引用的 cluster %q 不存在", contextName, ctx.Cluster)
	}
	user := cfg.findUser(ctx.User)

	creds := &credentials{
		ServerURL:  cluster.Server,
		Insecure:   cluster.InsecureSkipTLSVerify || s.Insecure,
		ServerName: cluster.TLSServerName,
		Context:    contextName,
		AuthInfo:   ctx.User,
	}
	if creds.ServerURL == "" {
		return nil, fmt.Errorf("context %q 的 cluster 没有 server 地址", contextName)
	}

	if creds.CAPEM, err = readDataOrFile(cluster.CertificateAuthorityData, cluster.CertificateAuthority, baseDir); err != nil {
		return nil, err
	}

	if user != nil {
		creds.Token = user.Token
		if user.TokenFile != "" {
			creds.TokenFile = resolveRelative(user.TokenFile, baseDir)
		}
		if creds.CertPEM, err = readDataOrFile(user.ClientCertificateData, user.ClientCertificate, baseDir); err != nil {
			return nil, err
		}
		if creds.KeyPEM, err = readDataOrFile(user.ClientKeyData, user.ClientKey, baseDir); err != nil {
			return nil, err
		}
		if user.Exec != nil {
			if !s.AllowExec {
				return nil, fmt.Errorf(
					"context %q 需要 exec 凭据插件（%s）。它会执行本地命令，插件默认禁用；确认可信后可在连接设置里打开“允许 kubeconfig exec 凭据”",
					contextName, user.Exec.Command)
			}
			if err := applyExecCredential(user.Exec, creds); err != nil {
				return nil, err
			}
		}
	}

	if creds.Token == "" && creds.TokenFile == "" && len(creds.CertPEM) == 0 {
		return nil, fmt.Errorf("context %q 没有任何可用凭据（token / 客户端证书 / exec）", contextName)
	}
	return creds, nil
}

// applyExecCredential runs a kubeconfig user.exec plugin. It is only reached
// when the user explicitly enabled allow_exec on the connection.
func applyExecCredential(execCfg *execConfig, creds *credentials) error {
	if strings.EqualFold(execCfg.InteractiveMode, "Always") {
		return errors.New("该 kubeconfig 的 exec 插件要求交互式登录，DBX 插件后端不代为执行；请先在终端完成登录")
	}
	if strings.TrimSpace(execCfg.Command) == "" {
		return errors.New("kubeconfig 的 exec 配置缺少 command")
	}
	timeout := 20 * time.Second
	cmd := exec.Command(execCfg.Command, execCfg.Args...)
	cmd.Env = os.Environ()
	for _, item := range execCfg.Env {
		cmd.Env = append(cmd.Env, item.Name+"="+item.Value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("执行 exec 凭据插件失败：%w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("exec 凭据插件退出异常：%v（stderr 已省略）", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return errors.New("exec 凭据插件超时")
	}

	var credential execCredential
	if err := json.Unmarshal(stdout.Bytes(), &credential); err != nil {
		return fmt.Errorf("exec 凭据插件输出不是 ExecCredential JSON：%w", err)
	}
	if credential.Status.Token != "" {
		creds.Token = credential.Status.Token
		creds.TokenFile = ""
	}
	if credential.Status.ClientCertificateData != "" && credential.Status.ClientKeyData != "" {
		cert, err := decodeBase64(credential.Status.ClientCertificateData)
		if err != nil {
			return err
		}
		key, err := decodeBase64(credential.Status.ClientKeyData)
		if err != nil {
			return err
		}
		creds.CertPEM = cert
		creds.KeyPEM = key
	}
	if creds.Token == "" && len(creds.CertPEM) == 0 {
		return errors.New("exec 凭据插件没有返回 token 或客户端证书")
	}
	return nil
}

func readDataOrFile(dataValue, pathValue, baseDir string) ([]byte, error) {
	if strings.TrimSpace(dataValue) != "" {
		return decodeBase64(dataValue)
	}
	if strings.TrimSpace(pathValue) == "" {
		return nil, nil
	}
	path := resolveRelative(pathValue, baseDir)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取凭据文件失败 %s：%w", path, err)
	}
	return raw, nil
}

func decodeBase64(value string) ([]byte, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, value)
	decoded, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return nil, errors.New("kubeconfig 中的 base64 数据无法解码")
	}
	return decoded, nil
}

func resolveRelative(path, baseDir string) string {
	path = expandHome(strings.TrimSpace(path))
	if filepath.IsAbs(path) || baseDir == "" {
		return path
	}
	return filepath.Join(baseDir, path)
}

// describeKubeconfig powers the "Use current context" dialog action and the
// kube/contexts method: it never exposes credential material.
func describeKubeconfig(s *settings) (contexts []string, current string, err error) {
	raw, _, readErr := readKubeconfig(s)
	if readErr != nil {
		return nil, "", readErr
	}
	cfg, parseErr := parseKubeconfig(raw)
	if parseErr != nil {
		return nil, "", parseErr
	}
	return cfg.contextNames(), cfg.CurrentContext, nil
}
