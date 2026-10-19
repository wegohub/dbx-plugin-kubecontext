package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
	"gopkg.in/yaml.v3"
)

// contextInfo is one kube context of the connection's aggregate kubeconfig.
type contextInfo struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Server    string `json:"server"`
	Namespace string `json:"namespace,omitempty"`
	User      string `json:"user,omitempty"`
	NeedsExec bool   `json:"needsExec,omitempty"`
	Current   bool   `json:"current"`
}

const maxKubeconfigBytes = 1 << 20

// targetPath is the kubeconfig file this connection reads, writes and merges
// into. It defaults to ~/.kube/config, i.e. the same file the local kubectl
// uses, which is what lets `kubectl get pod` work without --context.
func targetPath(s *settings) (string, error) {
	if strings.TrimSpace(s.KubeconfigPath) != "" {
		return expandHome(s.KubeconfigPath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

func readKubeconfigFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{"apiVersion": "v1", "kind": "Config"}, nil
		}
		return nil, err
	}
	doc := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("解析 %s 失败：%w", path, err)
		}
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func writeKubeconfigFile(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		backup := path + ".bak-" + time.Now().Format("20060102-150405.000000000")
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("备份原文件失败：%w", readErr)
		}
		if readErr == nil {
			if writeErr := os.WriteFile(backup, raw, 0o600); writeErr != nil {
				return fmt.Errorf("备份 %s 失败：%w", backup, writeErr)
			}
		}
	}
	encoded, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kubeconfig-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

func asObjectList(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			return typed
		}
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func nameOf(item map[string]any) string {
	if name, ok := item["name"].(string); ok {
		return name
	}
	return ""
}

// mergeKubeconfig merges clusters/contexts/users by name. Incoming entries win
// on a name clash, which matches what `kubectl config view --flatten` users
// expect when they re-import a refreshed file from ops.
func mergeKubeconfig(target, incoming map[string]any) (added, replaced []string) {
	for _, section := range []string{"clusters", "contexts", "users"} {
		targetList := asObjectList(target[section])
		index := map[string]int{}
		for i, item := range targetList {
			index[nameOf(item)] = i
		}
		for _, item := range asObjectList(incoming[section]) {
			name := nameOf(item)
			if name == "" {
				continue
			}
			label := fmt.Sprintf("%s/%s", section[:len(section)-1], name)
			if position, ok := index[name]; ok {
				targetList[position] = item
				replaced = append(replaced, label)
				continue
			}
			targetList = append(targetList, item)
			index[name] = len(targetList) - 1
			added = append(added, label)
		}
		list := make([]any, len(targetList))
		for i, item := range targetList {
			list[i] = item
		}
		target[section] = list
	}
	if current, ok := incoming["current-context"].(string); ok && current != "" {
		if existing, _ := target["current-context"].(string); existing == "" {
			target["current-context"] = current
		}
	}
	return added, replaced
}

// contextDetails describes every context of the connection's kubeconfig.
func contextDetails(s *settings) ([]contextInfo, string, string, error) {
	if s.AuthMode == "token" {
		return []contextInfo{{Name: "token", Cluster: s.Name, Server: s.Server, Namespace: s.Namespace, Current: true}}, "token", "", nil
	}
	path, err := targetPath(s)
	if err != nil {
		return nil, "", "", err
	}
	doc, readErr := readKubeconfigFile(path)
	if readErr != nil {
		return nil, "", path, readErr
	}
	current, _ := doc["current-context"].(string)

	clusters := map[string]map[string]any{}
	for _, item := range asObjectList(doc["clusters"]) {
		clusters[nameOf(item)] = item
	}
	users := map[string]map[string]any{}
	for _, item := range asObjectList(doc["users"]) {
		users[nameOf(item)] = item
	}

	contexts := make([]contextInfo, 0, len(asObjectList(doc["contexts"])))
	for _, item := range asObjectList(doc["contexts"]) {
		name := nameOf(item)
		if name == "" {
			continue
		}
		detail, _ := item["context"].(map[string]any)
		info := contextInfo{Name: name, Current: name == current}
		if detail != nil {
			info.Cluster, _ = detail["cluster"].(string)
			info.User, _ = detail["user"].(string)
			info.Namespace, _ = detail["namespace"].(string)
		}
		if cluster := clusters[info.Cluster]; cluster != nil {
			if body, ok := cluster["cluster"].(map[string]any); ok {
				info.Server, _ = body["server"].(string)
			}
		}
		if user := users[info.User]; user != nil {
			if body, ok := user["user"].(map[string]any); ok {
				if _, hasExec := body["exec"]; hasExec {
					info.NeedsExec = true
				}
			}
		}
		contexts = append(contexts, info)
	}
	sort.Slice(contexts, func(i, j int) bool { return contexts[i].Name < contexts[j].Name })
	return contexts, current, path, nil
}

func writeCurrentContext(s *settings, name string) (string, error) {
	path, err := targetPath(s)
	if err != nil {
		return "", err
	}
	doc, readErr := readKubeconfigFile(path)
	if readErr != nil {
		return "", readErr
	}
	known := false
	for _, item := range asObjectList(doc["contexts"]) {
		if nameOf(item) == name {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("kubeconfig 里没有 context %q", name)
	}
	if stringValue(doc, "current-context") == name {
		return path, nil
	}
	doc["current-context"] = name
	if err := writeKubeconfigFile(path, doc); err != nil {
		return "", err
	}
	return path, nil
}

// handleContexts lists the contexts of the connection's aggregate kubeconfig.
func (p *plugin) handleContexts(params map[string]any) (any, *dbx.PluginError) {
	base, _, perr := p.configFor(stringValue(params, "connectionId"))
	if perr != nil {
		return nil, perr
	}
	contexts, current, path, err := contextDetails(base)
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	return map[string]any{
		"contexts": contexts,
		"current":  current,
		"path":     path,
		"name":     base.Name,
		"selected": firstNonEmpty(base.Context, current),
		"authMode": base.AuthMode,
	}, nil
}

// handleMerge folds a dropped/selected kubeconfig into the target file, which is
// the same file kubectl reads. This is the "运维给我一个文件" entry point.
func (p *plugin) handleMerge(params map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	base, _, perr := p.configFor(stringValue(params, "connectionId"))
	if perr != nil {
		return nil, perr
	}
	text, _ := params["kubeconfig"].(string)
	result, perr := p.mergeInto(base, text, stringValue(params, "sourcePath"))
	if perr == nil {
		p.notifyFileChanged(base, emitter)
	}
	return result, perr
}

func (p *plugin) mergeInto(base *settings, text, source string) (map[string]any, *dbx.PluginError) {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	if base.AuthMode == "token" {
		return nil, dbx.NewError(-32602, "请使用 kubeconfig 文件连接导入")
	}
	if strings.TrimSpace(text) == "" {
		return nil, dbx.NewError(-32602, "kubeconfig 内容为空")
	}
	if len(text) > maxKubeconfigBytes {
		return nil, dbx.NewError(-32602, "kubeconfig 超过 1 MiB，请确认拖入的是正确的文件")
	}
	incoming := map[string]any{}
	if err := yaml.Unmarshal([]byte(text), &incoming); err != nil {
		return nil, dbx.NewError(-32602, "解析 kubeconfig 失败："+err.Error())
	}
	if len(asObjectList(incoming["contexts"])) == 0 {
		return nil, dbx.NewError(-32602, "这个文件里没有任何 context，确认是 kubeconfig 吗？")
	}

	if source != "" {
		absolutizeReferences(incoming, filepath.Dir(expandHome(source)))
	}
	path, err := targetPath(base)
	if err != nil {
		return nil, dbx.NewError(-32603, err.Error())
	}
	target, readErr := readKubeconfigFile(path)
	if readErr != nil {
		return nil, dbx.NewError(-32602, readErr.Error())
	}
	if source == "" && hasRelativeReferences(incoming) {
		return nil, dbx.NewError(-32602, "配置引用了相对路径，请在新建连接中选择源文件导入，或使用内嵌证书配置")
	}
	added, replaced := mergeKubeconfig(target, incoming)
	clusters := map[string]bool{}
	users := map[string]bool{}
	for _, item := range asObjectList(target["clusters"]) {
		clusters[nameOf(item)] = true
	}
	for _, item := range asObjectList(target["users"]) {
		users[nameOf(item)] = true
	}
	for _, item := range asObjectList(target["contexts"]) {
		context := asMap(item["context"])
		if nameOf(item) == "" || !clusters[stringValue(context, "cluster")] {
			return nil, dbx.NewError(-32602, "context 缺少有效的 cluster："+nameOf(item))
		}
		if user := stringValue(context, "user"); user != "" && !users[user] {
			return nil, dbx.NewError(-32602, "context 引用了不存在的 user："+nameOf(item))
		}
	}
	if writeErr := writeKubeconfigFile(path, target); writeErr != nil {
		return nil, dbx.NewError(-32603, "写入 "+path+" 失败："+writeErr.Error())
	}

	p.mu.Lock()
	for id, config := range p.configs {
		otherPath, _ := targetPath(config)
		if filepath.Clean(otherPath) == filepath.Clean(path) {
			for key := range p.sessions {
				if strings.HasPrefix(key, id+"\x00") {
					delete(p.sessions, key)
				}
			}
		}
	}
	p.mu.Unlock()
	contexts, current, _, listErr := contextDetails(base)
	if listErr != nil {
		return nil, dbx.NewError(-32603, listErr.Error())
	}
	return map[string]any{
		"path":     path,
		"name":     base.Name,
		"added":    added,
		"replaced": replaced,
		"contexts": contexts,
		"current":  current,
	}, nil
}

// handleUseContext changes the kubectl default without dialing the cluster.
func (p *plugin) handleUseContext(params map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	base, _, perr := p.configFor(stringValue(params, "connectionId"))
	if perr != nil {
		return nil, perr
	}
	name := stringValue(params, "context")
	if name == "" {
		return nil, dbx.NewError(-32602, "缺少 context 名称")
	}
	if base.AuthMode == "token" {
		return map[string]any{"success": true, "current": "token"}, nil
	}
	path, err := writeCurrentContext(base, name)
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	p.notifyFileChanged(base, emitter)
	return map[string]any{"success": true, "current": name, "path": path}, nil
}

// Moving a kubeconfig must preserve the base of certificate and token paths.
func absolutizeReferences(doc map[string]any, base string) {
	for _, section := range []string{"clusters", "users"} {
		key := "cluster"
		fields := []string{"certificate-authority"}
		if section == "users" {
			key = "user"
			fields = []string{"client-certificate", "client-key", "tokenFile"}
		}
		for _, entry := range asObjectList(doc[section]) {
			body := asMap(entry[key])
			for _, field := range fields {
				path := stringValue(body, field)
				if path != "" && !filepath.IsAbs(path) {
					body[field] = filepath.Join(base, path)
				}
			}
			if exec := asMap(body["exec"]); len(exec) > 0 {
				command := stringValue(exec, "command")
				if strings.Contains(command, string(filepath.Separator)) && !filepath.IsAbs(command) {
					exec["command"] = filepath.Join(base, command)
				}
			}
		}
	}
}

func hasRelativeReferences(doc map[string]any) bool {
	for _, section := range []string{"clusters", "users"} {
		key := "cluster"
		fields := []string{"certificate-authority"}
		if section == "users" {
			key = "user"
			fields = []string{"client-certificate", "client-key", "tokenFile"}
		}
		for _, entry := range asObjectList(doc[section]) {
			body := asMap(entry[key])
			for _, field := range fields {
				if path := stringValue(body, field); path != "" && !filepath.IsAbs(path) {
					return true
				}
			}
			command := stringValue(asMap(body["exec"]), "command")
			if strings.Contains(command, string(filepath.Separator)) && !filepath.IsAbs(command) {
				return true
			}
		}
	}
	return false
}

// Connections may share a kubeconfig file. An import from a new draft must
// refresh existing connections to that same file as well.
func (p *plugin) notifyFileChanged(base *settings, emitter *dbx.Emitter) {
	target, _ := targetPath(base)
	ids := map[string]bool{base.ConnectionID: true}
	p.mu.Lock()
	for id, config := range p.configs {
		path, err := targetPath(config)
		if err == nil && filepath.Clean(path) == filepath.Clean(target) {
			ids[id] = true
		}
	}
	p.mu.Unlock()
	for id := range ids {
		_ = emitter.Event("kube/contextsChanged", map[string]any{"connectionId": id})
	}
}
