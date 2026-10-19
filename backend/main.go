package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

const (
	pluginID      = "com.example.kubecontext"
	pluginVersion = "0.5.1"
)

// session is one kube context of one connection.
type session struct {
	settings *settings
	client   *kubeClient
	runtime  map[string]any
}

func (sess *session) kube() (*kubeClient, *dbx.PluginError) {
	if sess.client == nil {
		return nil, dbx.NewError(-32011, "还没有可用的集群连接：请先导入 kubeconfig")
	}
	return sess.client, nil
}

type plugin struct {
	fileMu   sync.Mutex
	mu       sync.RWMutex
	configs  map[string]*settings
	runtimes map[string]map[string]any
	sessions map[string]*session
	streams  map[string]*activeStream
}

func newPlugin() *plugin {
	return &plugin{
		configs:  map[string]*settings{},
		runtimes: map[string]map[string]any{},
		sessions: map[string]*session{},
		streams:  map[string]*activeStream{},
	}
}

func main() {
	log.SetPrefix("kubecontext: ")
	log.SetFlags(0)
	server := dbx.NewServer(
		dbx.Metadata{
			ID:           pluginID,
			Version:      pluginVersion,
			Capabilities: []string{"connections", "events", "mcp"},
		},
		newPlugin(),
	)
	if err := server.Serve(); err != nil {
		log.Fatal(err)
	}
}

func (p *plugin) Handle(_ dbx.RequestContext, method string, params json.RawMessage, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	payload := map[string]any{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, dbx.NewError(-32602, "无法解析请求参数："+err.Error())
		}
	}

	switch method {
	case "connection/test":
		return p.connectionTest(payload)
	case "connection/connect":
		return p.connectionConnect(payload, emitter)
	case "connection/disconnect":
		return p.connectionDisconnect(payload, emitter)
	case "connection/action":
		return p.connectionAction(payload, emitter)
	case "kube/contexts":
		return p.handleContexts(payload)
	case "kube/merge":
		return p.handleMerge(payload, emitter)
	case "kube/use-context":
		return p.handleUseContext(payload, emitter)
	case "kube/pod-metrics":
		return p.handlePodMetrics(payload)
	case "kube/pod-usage":
		return p.handlePodUsage(payload)
	case "kube/overview":
		return p.handleOverview(payload)
	case "kube/permissions":
		return p.handlePermissions(payload)
	case "kube/list":
		return p.handleList(payload)
	case "kube/get":
		return p.handleGet(payload)
	case "kube/logs":
		return p.handleLogs(payload)
	case "kube/logs/start":
		return p.handleLogsStart(payload, emitter)
	case "kube/logs/stop":
		return p.handleLogsStop(payload)
	case "kube/events":
		return p.handleRelatedEvents(payload)
	case "kube/scale", "kube/restart", "kube/delete":
		return nil, dbx.NewError(-32013, "插件为资源只读模式，已禁用删除、扩缩容和重启")
	case "terminal/start":
		return p.handleTerminalStart(payload, emitter)
	case "terminal/input":
		return p.handleTerminalInput(payload)
	case "terminal/resize":
		return p.handleTerminalResize(payload)
	case "terminal/stop":
		return p.handleTerminalStop(payload)
	case "mcp/tools":
		return mcpTools(payload)
	case "mcp/call":
		return p.mcpCall(payload)
	default:
		return nil, dbx.MethodNotFound(method)
	}
}

func (p *plugin) connectionTest(payload map[string]any) (any, *dbx.PluginError) {
	s, err := settingsFromConnection(asMap(payload["connection"]))
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	if s.AuthMode == "kubeconfig" {
		contexts, _, _, err := contextDetails(s)
		if err != nil {
			return nil, dbx.NewError(-32602, err.Error())
		}
		return map[string]any{"success": true, "message": fmt.Sprintf("kubeconfig 可读取 · %d 个 context（进入时连接集群）", len(contexts))}, nil
	}
	client, buildErr := buildClient(s, asMap(payload["runtime"]))
	if buildErr != nil {
		return nil, dbx.NewError(-32602, buildErr.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Timeout)
	defer cancel()
	version, probeErr := client.serverVersion(ctx)
	if probeErr != nil {
		return map[string]any{"success": false, "message": probeErr.Error()}, nil
	}
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("%s · %s", version["gitVersion"], currentContextLabel(s)),
	}, nil
}

func currentContextLabel(s *settings) string {
	if name, err := currentContextName(s); err == nil {
		return name
	}
	return s.Name
}

func (p *plugin) connectionConnect(payload map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	s, err := settingsFromConnection(asMap(payload["connection"]))
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	p.rememberConfig(s.ConnectionID, s, asMap(payload["runtime"]))

	if s.AuthMode == "kubeconfig" {
		if _, _, _, err := contextDetails(s); err != nil {
			return nil, dbx.NewError(-32602, err.Error())
		}
		_ = emitter.Event("kube/connectionChanged", map[string]any{"connectionId": s.ConnectionID, "state": "connected"})
		return map[string]any{"success": true, "message": "已打开 kubeconfig，请选择 context"}, nil
	}

	sess, sessErr := p.sessionForContext(s.ConnectionID, "")
	if sessErr != nil {
		return nil, sessErr
	}
	_ = emitter.Event("kube/connectionChanged", map[string]any{
		"connectionId": s.ConnectionID,
		"state":        "connected",
		"context":      sess.settings.Context,
	})
	return map[string]any{"success": true, "message": "已连接 " + sess.settings.Context}, nil
}

func (p *plugin) connectionDisconnect(payload map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	connectionID := stringValue(asMap(payload["connection"]), "id")
	if connectionID == "" {
		return nil, dbx.NewError(-32602, "缺少连接 id")
	}
	p.cancelStreamsFor(connectionID)
	p.dropConnection(connectionID)
	_ = emitter.Event("kube/connectionChanged", map[string]any{
		"connectionId": connectionID,
		"state":        "disconnected",
	})
	return map[string]any{"success": true}, nil
}

// connectionAction backs the "Use current context" button in the connection
// dialog: declarative forms cannot load a dynamic dropdown, so the backend
// resolves current-context and writes it back into the text field.
func (p *plugin) connectionAction(payload map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	actionID := stringValue(asMap(payload["action"]), "id")
	if actionID == "import-kubeconfig" {
		s, err := settingsFromConnection(asMap(payload["connection"]))
		if err != nil {
			return nil, dbx.NewError(-32602, err.Error())
		}
		config := asMap(asMap(payload["connection"])["external_config"])
		secrets := asMap(asMap(payload["connection"])["connection_secrets"])
		text := stringValue(secrets, "import_content")
		source := expandHome(stringValue(config, "import_path"))
		if source != "" {
			file, readErr := os.Open(source)
			if readErr != nil {
				return nil, dbx.NewError(-32602, readErr.Error())
			}
			defer file.Close()
			raw, readErr := io.ReadAll(io.LimitReader(file, maxKubeconfigBytes+1))
			if readErr != nil {
				return nil, dbx.NewError(-32602, readErr.Error())
			}
			text = string(raw)
		}
		result, perr := p.mergeInto(s, text, source)
		if perr != nil {
			return nil, perr
		}
		p.notifyFileChanged(s, emitter)
		return map[string]any{"success": true, "message": "已合并到 " + result["path"].(string) + "，保存连接后在工作台顶部选择 context", "fieldValues": map[string]any{"import_path": "", "import_content": ""}}, nil
	}
	if actionID != "detect-context" {
		return nil, dbx.NewError(-32602, "未知的连接操作："+actionID)
	}
	s, err := settingsFromConnection(asMap(payload["connection"]))
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	contexts, current, describeErr := describeKubeconfig(s)
	if describeErr != nil {
		return map[string]any{"success": false, "message": describeErr.Error()}, nil
	}
	if current == "" && len(contexts) > 0 {
		current = contexts[0]
	}
	return map[string]any{
		"success":     true,
		"message":     fmt.Sprintf("当前 context：%s（共 %d 个）", current, len(contexts)),
		"fieldValues": map[string]any{"context": current},
	}, nil
}

func mcpTools(_ map[string]any) (any, *dbx.PluginError) {
	kinds := make([]any, 0, len(kindCatalog))
	for key := range kindCatalog {
		kinds = append(kinds, key)
	}
	tools := []map[string]any{
		{
			"name":        "list_resources",
			"description": "List Kubernetes resources of one kind, optionally filtered by namespace or label selector.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"connectionId":  map[string]any{"type": "string", "description": "DBX connection id"},
					"context":       map[string]any{"type": "string", "description": "kube context"},
					"kind":          map[string]any{"type": "string", "enum": kinds},
					"namespace":     map[string]any{"type": "string"},
					"labelSelector": map[string]any{"type": "string"},
				},
				"required": []any{"kind"},
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "get_resource",
			"description": "Fetch the full JSON of one Kubernetes object.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"connectionId": map[string]any{"type": "string"},
					"context":      map[string]any{"type": "string"},
					"kind":         map[string]any{"type": "string", "enum": kinds},
					"namespace":    map[string]any{"type": "string"},
					"name":         map[string]any{"type": "string"},
				},
				"required": []any{"kind", "name"},
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
		{
			"name":        "pod_logs",
			"description": "Read the tail of a pod container log.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"connectionId": map[string]any{"type": "string"},
					"context":      map[string]any{"type": "string"},
					"namespace":    map[string]any{"type": "string"},
					"name":         map[string]any{"type": "string"},
					"container":    map[string]any{"type": "string"},
					"tailLines":    map[string]any{"type": "number"},
				},
				"required": []any{"name"},
			},
			"annotations": map[string]any{"readOnlyHint": true},
		},
	}
	return map[string]any{"tools": tools}, nil
}

func (p *plugin) mcpCall(payload map[string]any) (any, *dbx.PluginError) {
	tool := stringValue(payload, "tool")
	args := asMap(payload["arguments"])
	var (
		result any
		perr   *dbx.PluginError
	)
	switch tool {
	case "list_resources":
		result, perr = p.handleList(args)
	case "get_resource":
		result, perr = p.handleGet(args)
	case "pod_logs":
		result, perr = p.handleLogs(args)
	default:
		return nil, dbx.NewError(-32602, "未知的 MCP 工具："+tool)
	}
	if perr != nil {
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": perr.Message}},
			"isError": true,
		}, nil
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, dbx.NewError(-32603, err.Error())
	}
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": string(encoded)}},
		"isError": false,
	}, nil
}
