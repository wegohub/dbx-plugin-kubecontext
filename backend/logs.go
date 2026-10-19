package main

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// handleLogsStart streams a pod log with follow=true and pushes batches to the
// workbench as `kube/logChunk` events, so the log pane renders live instead of
// polling a tail.
func (p *plugin) handleLogsStart(params map[string]any, emitter *dbx.Emitter) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}

	namespace := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	pod := stringValue(params, "name")
	if pod == "" {
		return nil, dbx.NewError(-32602, "缺少 Pod 名称")
	}
	container := stringValue(params, "container")
	query := url.Values{}
	query.Set("follow", "true")
	query.Set("tailLines", strconv.Itoa(clampInt(intValue(params, "tailLines", 100), 1, 5000)))
	if boolValue(params, "previous") {
		query.Set("previous", "true")
	}
	if boolValue(params, "timestamps") {
		query.Set("timestamps", "true")
	}
	if container != "" {
		query.Set("container", container)
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(pod) + "/log?" + query.Encode()

	ctx, cancel := context.WithCancel(context.Background())
	streamID := stringValue(params, "streamId")
	if streamID == "" {
		streamID = fmt.Sprintf("log-%d", time.Now().UnixNano())
	}
	p.mu.Lock()
	if _, exists := p.streams[streamID]; exists {
		p.mu.Unlock()
		cancel()
		return nil, dbx.NewError(-32602, "streamId 已存在")
	}
	p.streams[streamID] = &activeStream{connectionID: sess.settings.ConnectionID, cancel: cancel}
	p.mu.Unlock()

	go func() {
		defer p.unregisterStream(streamID)
		defer cancel()
		resp, err := client.openStream(ctx, path)
		if err != nil {
			_ = emitter.Event("kube/logError", map[string]any{"streamId": streamID, "message": err.Error()})
			return
		}
		defer func() { _ = resp.Body.Close() }()

		lines := make(chan string, 256)
		scanErr := make(chan error, 1)
		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(resp.Body)
			scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
			defer func() { scanErr <- scanner.Err() }()
			for scanner.Scan() {
				select {
				case lines <- stripANSI(scanner.Text()):
				case <-ctx.Done():
					return
				}
			}
		}()

		batch := make([]string, 0, 32)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			_ = emitter.Event("kube/logChunk", map[string]any{"streamId": streamID, "lines": batch})
			batch = batch[:0]
		}
		ticker := time.NewTicker(80 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					flush()
					if err := <-scanErr; err != nil && ctx.Err() == nil {
						_ = emitter.Event("kube/logError", map[string]any{"streamId": streamID, "message": err.Error()})
						return
					}
					_ = emitter.Event("kube/logEnd", map[string]any{"streamId": streamID})
					return
				}
				batch = append(batch, line)
				if len(batch) >= 20 {
					flush()
				}
			case <-ticker.C:
				flush()
			case <-ctx.Done():
				flush()
				_ = emitter.Event("kube/logEnd", map[string]any{"streamId": streamID, "cancelled": true})
				return
			}
		}
	}()

	return map[string]any{"streamId": streamID, "namespace": namespace, "pod": pod}, nil
}

func (p *plugin) handleLogsStop(params map[string]any) (any, *dbx.PluginError) {
	streamID := stringValue(params, "streamId")
	if streamID == "" {
		return nil, dbx.NewError(-32602, "缺少 streamId")
	}
	return map[string]any{"success": true, "stopped": p.cancelStream(streamID)}, nil
}
