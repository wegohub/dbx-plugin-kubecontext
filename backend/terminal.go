package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// Each shell gets a private kubeconfig snapshot. Changing the global default
// in another tab cannot silently redirect an already-open terminal.
func (p *plugin) terminalConfig(base *settings, name string) (string, func(), error) {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	source, err := targetPath(base)
	if err != nil {
		return "", nil, err
	}
	doc := map[string]any{}
	if base.AuthMode != "token" {
		doc, err = readKubeconfigFile(source)
		if err != nil {
			return "", nil, err
		}
	}
	if base.AuthMode == "token" {
		server := base.Server
		if base.Port > 0 {
			server = withPort(server, base.Port)
		}
		server, _, err = normalizeServer(server, "")
		if err != nil {
			return "", nil, err
		}
		doc = map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": []any{map[string]any{"name": "token", "cluster": map[string]any{"server": server, "certificate-authority-data": base64.StdEncoding.EncodeToString([]byte(base.CA)), "insecure-skip-tls-verify": base.Insecure}}}, "contexts": []any{map[string]any{"name": "token", "context": map[string]any{"cluster": "token", "user": "token", "namespace": base.Namespace}}}, "users": []any{map[string]any{"name": "token", "user": map[string]any{"token": base.Token}}}}
	}
	known := false
	for _, item := range asObjectList(doc["contexts"]) {
		if nameOf(item) == name {
			known = true
		}
	}
	if !known {
		return "", nil, fmt.Errorf("kubeconfig 里没有 context %q", name)
	}
	doc["current-context"] = name
	absolutizeReferences(doc, filepath.Dir(source))
	dir, err := os.MkdirTemp("", "dbx-kubecontext-terminal-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "config")
	if err := writeKubeconfigFile(path, doc); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func (p *plugin) handleTerminalStart(params map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	base, _, perr := p.configFor(stringValue(params, "connectionId"))
	if perr != nil {
		return nil, perr
	}
	name := stringValue(params, "context")
	if name == "" {
		return nil, dbx.NewError(-32602, "请选择终端的 context")
	}
	path, cleanup, err := p.terminalConfig(base, name)
	if err != nil {
		return nil, dbx.NewError(-32602, err.Error())
	}
	cmd, perr := p.terminalCommand(params, path)
	if perr != nil {
		cleanup()
		return nil, perr
	}
	cmd.Dir, _ = os.UserHomeDir()
	env := []string{}
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "KUBECONFIG=") || strings.HasPrefix(value, "TERM=") {
			continue
		}
		env = append(env, value)
	}
	cmd.Env = append(env, "KUBECONFIG="+path, "TERM=xterm-256color")
	size := terminalSize(params)
	terminal, err := pty.StartWithSize(cmd, size)
	if err != nil {
		cleanup()
		return nil, dbx.NewError(-32603, "启动本地终端失败："+err.Error())
	}
	id := stringValue(params, "streamId")
	if id == "" {
		id = fmt.Sprintf("term-%d", time.Now().UnixNano())
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() {
		cancel()
		_ = terminal.Close()
		// Closing the PTY hangs up its foreground job; also terminate the shell group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}
	p.mu.Lock()
	if _, exists := p.streams[id]; exists {
		p.mu.Unlock()
		stop()
		_ = cmd.Wait()
		cleanup()
		return nil, dbx.NewError(-32602, "streamId 已存在")
	}
	p.streams[id] = &activeStream{connectionID: base.ConnectionID, cancel: stop, terminal: terminal}
	p.mu.Unlock()
	go func() {
		defer cleanup()
		defer cancel()
		defer terminal.Close()
		defer p.unregisterStream(id)
		chars := make(chan rune, 4096)
		go func() {
			defer close(chars)
			reader := bufio.NewReader(terminal)
			for {
				char, _, err := reader.ReadRune()
				if err != nil {
					return
				}
				select {
				case chars <- char:
				case <-ctx.Done():
					return
				}
			}
		}()
		var batch strings.Builder
		flush := func() {
			if batch.Len() == 0 {
				return
			}
			_ = emitter.Event("terminal/output", map[string]any{"streamId": id, "data": batch.String()})
			batch.Reset()
		}
		ticker := time.NewTicker(16 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case char, ok := <-chars:
				if !ok {
					flush()
					code := 0
					if err := cmd.Wait(); err != nil {
						var exitErr *exec.ExitError
						if errors.As(err, &exitErr) {
							code = exitErr.ExitCode()
						} else {
							code = -1
						}
					}
					_ = emitter.Event("terminal/exit", map[string]any{"streamId": id, "code": code})
					return
				}
				batch.WriteRune(char)
				if batch.Len() >= 4096 {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()
	return map[string]any{"streamId": id, "context": name}, nil
}

// Build an argv directly; pod/container names and shell choices never enter a shell command string.
func (p *plugin) terminalCommand(params map[string]any, config string) (*exec.Cmd, *dbx.PluginError) {
	pod := stringValue(params, "pod")
	if pod == "" {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		return exec.Command(shell, "-il"), nil
	}
	shell := firstNonEmpty(stringValue(params, "shell"), "/bin/sh")
	if shell != "/bin/sh" && shell != "/bin/bash" {
		return nil, dbx.NewError(-32602, "请选择 sh 或 bash")
	}
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	ns := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	if ns == "*" || strings.HasPrefix(pod, "-") {
		return nil, dbx.NewError(-32602, "进入容器需要明确的 Pod 和命名空间")
	}
	client, perr := sess.kube()
	if perr != nil {
		return nil, perr
	}
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout)
	defer cancel()
	var object map[string]any
	if err := client.getJSON(ctx, kindCatalog["pods"].objectPath(ns, pod), &object); err != nil {
		return nil, toPluginError(err)
	}
	container := stringValue(params, "container")
	valid := false
	for _, section := range []string{"containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"} {
		for _, status := range mapListAt(object, "status", section) {
			if textAt(status, "name") == container && dig(status, "state", "running") != nil {
				valid = true
			}
		}
	}
	if !valid || strings.HasPrefix(container, "-") {
		return nil, dbx.NewError(-32602, "这个容器当前没有运行，请刷新 Pod 状态")
	}
	return exec.Command("kubectl", "--kubeconfig", config, "--namespace", ns, "exec", "-it", pod, "-c", container, "--", shell), nil
}

func terminalSize(params map[string]any) *pty.Winsize {
	return &pty.Winsize{Cols: uint16(clampInt(intValue(params, "cols", 80), 2, 500)), Rows: uint16(clampInt(intValue(params, "rows", 24), 2, 300))}
}

func (p *plugin) terminalFor(params map[string]any) (*os.File, *dbx.PluginError) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stream := p.streams[stringValue(params, "streamId")]
	if stream == nil || stream.terminal == nil || stream.connectionID != stringValue(params, "connectionId") {
		return nil, dbx.NewError(-32602, "终端已关闭")
	}
	return stream.terminal, nil
}
func (p *plugin) handleTerminalInput(params map[string]any) (any, *dbx.PluginError) {
	terminal, perr := p.terminalFor(params)
	if perr != nil {
		return nil, perr
	}
	data, _ := params["data"].(string) // Do not trim input: spaces/CR/Ctrl-C are meaningful.
	if len(data) > 65536 {
		return nil, dbx.NewError(-32602, "输入过长")
	}
	if _, err := terminal.WriteString(data); err != nil {
		return nil, dbx.NewError(-32603, err.Error())
	}
	return map[string]any{"success": true}, nil
}
func (p *plugin) handleTerminalResize(params map[string]any) (any, *dbx.PluginError) {
	terminal, perr := p.terminalFor(params)
	if perr != nil {
		return nil, perr
	}
	if err := pty.Setsize(terminal, terminalSize(params)); err != nil {
		return nil, dbx.NewError(-32603, err.Error())
	}
	return map[string]any{"success": true}, nil
}
func (p *plugin) handleTerminalStop(params map[string]any) (any, *dbx.PluginError) {
	if _, perr := p.terminalFor(params); perr != nil {
		return map[string]any{"success": true}, nil
	}
	return map[string]any{"success": true, "stopped": p.cancelStream(stringValue(params, "streamId"))}, nil
}
