package main

import (
	"context"
	"errors"
	"strings"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// A connection is one kubeconfig file (an aggregate such as ~/.kube/config);
// a session is one kube context inside it. Sessions are created lazily so a
// single DBX connection can drive several contexts, with an isolated local
// terminal for each context inside the workbench.

func sessionKey(connectionID, contextName string) string {
	return connectionID + "\x00" + contextName
}

// hasCredentials reports whether the connection can dial a cluster without an
// import step (a kubeconfig file/content or an API server + token).
func hasCredentials(s *settings) bool {
	if s.AuthMode == "token" {
		return s.Server != "" && s.Token != ""
	}
	return s.KubeconfigPath != "" || s.Kubeconfig != ""
}

func (p *plugin) rememberConfig(connectionID string, s *settings, runtime map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.sessions {
		if strings.HasPrefix(key, connectionID+"\x00") {
			delete(p.sessions, key)
		}
	}
	p.configs[connectionID] = s
	p.runtimes[connectionID] = runtime
}

func (p *plugin) configFor(connectionID string) (*settings, map[string]any, *dbx.PluginError) {
	if connectionID == "" {
		return nil, nil, dbx.NewError(-32602, "缺少 connectionId")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	base, ok := p.configs[connectionID]
	if !ok {
		return nil, nil, dbx.NewError(-32010, "该连接尚未建立，请在 DBX 中先连接这个 KubeContext")
	}
	return base, p.runtimes[connectionID], nil
}

func (p *plugin) dropConnection(connectionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.configs, connectionID)
	delete(p.runtimes, connectionID)
	for key := range p.sessions {
		if strings.HasPrefix(key, connectionID+"\x00") {
			delete(p.sessions, key)
		}
	}
}

// session resolves the (connection, context) pair from a kube/* request.
func (p *plugin) session(params map[string]any) (*session, *dbx.PluginError) {
	return p.sessionForContext(stringValue(params, "connectionId"), stringValue(params, "context"))
}

// sessionForContext returns a connected session, building it on first use.
func (p *plugin) sessionForContext(connectionID, contextName string) (*session, *dbx.PluginError) {
	base, runtime, perr := p.configFor(connectionID)
	if perr != nil {
		return nil, perr
	}

	if contextName == "" {
		contextName = base.Context
	}
	if contextName == "" {
		resolved, err := currentContextName(base)
		if err != nil {
			return nil, dbx.NewError(-32011, err.Error())
		}
		contextName = resolved
	}

	key := sessionKey(connectionID, contextName)
	p.mu.RLock()
	cached := p.sessions[key]
	p.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	clone := *base
	clone.Context = contextName
	if clone.AuthMode != "token" {
		// A connection is an aggregate file. Its legacy namespace setting must
		// never override the namespace belonging to the selected context.
		clone.Namespace = firstNonEmpty(contextNamespace(&clone, contextName), "default")
	}
	client, err := buildClient(&clone, runtime)
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), clone.Timeout)
	defer cancel()
	if _, probeErr := client.serverVersion(ctx); probeErr != nil {
		return nil, dbx.NewError(-32603, "连接 context "+contextName+" 失败："+probeErr.Error())
	}

	created := &session{settings: &clone, client: client, runtime: runtime}
	p.mu.Lock()
	p.sessions[key] = created
	p.mu.Unlock()
	return created, nil
}

// currentContextName reads the connection's kubeconfig and returns its
// current-context, which is what the connection falls back to.
func currentContextName(s *settings) (string, error) {
	if s.AuthMode == "token" {
		if s.Server == "" {
			return "", errors.New("还没有可用的集群连接：请导入 kubeconfig 或填写 API Server")
		}
		return "token", nil
	}
	raw, _, err := readKubeconfig(s)
	if err != nil {
		return "", err
	}
	cfg, parseErr := parseKubeconfig(raw)
	if parseErr != nil {
		return "", parseErr
	}
	if cfg.CurrentContext != "" {
		return cfg.CurrentContext, nil
	}
	if len(cfg.Contexts) == 1 {
		return cfg.Contexts[0].Name, nil
	}
	return "", errors.New("kubeconfig 没有 current-context：请先在工作台里选一个 context")
}

// contextNamespace reads the namespace declared by one kube context.
func contextNamespace(s *settings, contextName string) string {
	raw, _, err := readKubeconfig(s)
	if err != nil {
		return ""
	}
	cfg, parseErr := parseKubeconfig(raw)
	if parseErr != nil {
		return ""
	}
	if ctx := cfg.findContext(contextName); ctx != nil {
		return ctx.Namespace
	}
	return ""
}
