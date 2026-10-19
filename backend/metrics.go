package main

import (
	"context"
	"net/url"
	"sync"
	"time"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

func metricsPath(namespace, name string) string {
	prefix := "/apis/metrics.k8s.io/v1beta1"
	if namespace != "" && namespace != "*" {
		prefix += "/namespaces/" + url.PathEscape(namespace)
	}
	prefix += "/pods"
	if name != "" {
		prefix += "/" + url.PathEscape(name)
	}
	return prefix
}
func metricsUsage(object map[string]any) (*float64, *float64) {
	cpu, memory := 0.0, 0.0
	hasCPU, hasMemory := false, false
	for _, container := range mapListAt(object, "containers") {
		if value := quantity(textAt(container, "usage", "cpu")); value != nil {
			cpu += *value * 1000
			hasCPU = true
		}
		if value := quantity(textAt(container, "usage", "memory")); value != nil {
			memory += *value
			hasMemory = true
		}
	}
	var c, m *float64
	if hasCPU {
		c = &cpu
	}
	if hasMemory {
		m = &memory
	}
	return c, m
}
func optionalNumber(value any) *float64 {
	switch n := value.(type) {
	case float64:
		return &n
	case int:
		v := float64(n)
		return &v
	}
	return nil
}
func metricError(err error) string {
	if err == nil {
		return ""
	}
	return toPluginError(err).Message
}
func (p *plugin) handlePodMetrics(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	name := stringValue(params, "name")
	if name == "" {
		return nil, dbx.NewError(-32602, "缺少 Pod 名称")
	}
	ns := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout)
	defer cancel()
	var pod map[string]any
	if err := client.getJSON(ctx, kindCatalog["pods"].objectPath(ns, name), &pod); err != nil {
		return nil, toPluginError(err)
	}
	result := map[string]any{"podUid": textAt(pod, "metadata", "uid"), "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "budget": resourceBudget(pod), "cpuMillicores": nil, "memoryBytes": nil, "diskUsedBytes": nil, "network": nil, "volumes": []any{}, "sources": map[string]string{}}
	sources := result["sources"].(map[string]string)
	var metrics, summary map[string]any
	var metricErr, summaryErr error
	var group sync.WaitGroup
	group.Add(1)
	go func() { defer group.Done(); metricErr = client.getJSON(ctx, metricsPath(ns, name), &metrics) }()
	node := textAt(pod, "spec", "nodeName")
	if node != "" {
		group.Add(1)
		go func() {
			defer group.Done()
			summaryErr = client.getJSON(ctx, "/api/v1/nodes/"+url.PathEscape(node)+"/proxy/stats/summary", &summary)
		}()
	}
	group.Wait()
	warnings := []map[string]string{}
	if metricErr == nil {
		cpu, memory := metricsUsage(metrics)
		result["cpuMillicores"] = cpu
		result["memoryBytes"] = memory
		result["metricsTimestamp"] = textAt(metrics, "timestamp")
		result["metricsWindow"] = textAt(metrics, "window")
		sources["cpuMemory"] = "metrics-server"
		containers := []map[string]any{}
		for _, item := range mapListAt(metrics, "containers") {
			cpu, memory := metricsUsage(map[string]any{"containers": []any{item}})
			containers = append(containers, map[string]any{"name": textAt(item, "name"), "cpuMillicores": cpu, "memoryBytes": memory})
		}
		result["containers"] = containers
	} else {
		warnings = append(warnings, map[string]string{"scope": "Metrics API", "message": metricError(metricErr)})
	}
	var selected map[string]any
	for _, item := range mapListAt(summary, "pods") {
		if textAt(item, "podRef", "name") != name || textAt(item, "podRef", "namespace") != ns {
			continue
		}
		if uid := textAt(item, "podRef", "uid"); uid != "" && textAt(pod, "metadata", "uid") != "" && uid != textAt(pod, "metadata", "uid") {
			continue
		}
		selected = item
		break
	}
	if selected != nil {
		if metricErr != nil {
			result["cpuMillicores"] = multiplied(optionalNumber(dig(selected, "cpu", "usageNanoCores")), 1e-6)
			result["memoryBytes"] = optionalNumber(dig(selected, "memory", "workingSetBytes"))
			sources["cpuMemory"] = "kubelet-summary"
			result["metricsTimestamp"] = textAt(selected, "cpu", "time")
		}
		result["diskUsedBytes"] = optionalNumber(dig(selected, "ephemeral-storage", "usedBytes"))
		result["diskTimestamp"] = textAt(selected, "ephemeral-storage", "time")
		sources["disk"] = "kubelet-summary"
		if network, ok := selected["network"].(map[string]any); ok {
			result["network"] = map[string]any{"timestamp": textAt(network, "time"), "rxBytes": optionalNumber(network["rxBytes"]), "txBytes": optionalNumber(network["txBytes"]), "rxErrors": optionalNumber(network["rxErrors"]), "txErrors": optionalNumber(network["txErrors"])}
			sources["network"] = "kubelet-summary"
		}
		result["volumes"] = mapListAt(selected, "volume")
	} else {
		message := "kubelet 未返回这个 Pod 的统计数据"
		if node == "" {
			message = "Pod 尚未调度到节点"
		}
		if summaryErr != nil {
			message = metricError(summaryErr)
		}
		warnings = append(warnings, map[string]string{"scope": "磁盘 / 网络", "message": message})
	}

	if selected != nil && metricErr != nil {
		containers := []map[string]any{}
		for _, item := range mapListAt(selected, "containers") {
			containers = append(containers, map[string]any{"name": textAt(item, "name"), "cpuMillicores": multiplied(optionalNumber(dig(item, "cpu", "usageNanoCores")), 1e-6), "memoryBytes": optionalNumber(dig(item, "memory", "workingSetBytes"))})
		}
		result["containers"] = containers
	}
	// Missing fields are unknown, never a synthetic zero.
	missing := func(value any) bool {
		if value == nil {
			return true
		}
		number, ok := value.(*float64)
		return ok && number == nil
	}
	if missing(result["cpuMillicores"]) || missing(result["memoryBytes"]) {
		warnings = append(warnings, map[string]string{"scope": "CPU / 内存", "message": "没有可用的 CPU 或内存采样，Metrics API 需要集群安装指标服务并授予读取权限"})
	}
	if selected != nil && (missing(result["diskUsedBytes"]) || result["network"] == nil) {
		warnings = append(warnings, map[string]string{"scope": "磁盘 / 网络", "message": "kubelet 返回的 Pod 统计中缺少磁盘或网络字段"})
	}
	result["warnings"] = warnings
	return result, nil
}
func (p *plugin) handlePodUsage(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	ns := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout)
	defer cancel()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.getJSON(ctx, metricsPath(ns, ""), &list); err != nil {
		return nil, toPluginError(err)
	}
	rows := []map[string]any{}
	for _, pod := range list.Items {
		cpu, memory := metricsUsage(pod)
		rows = append(rows, map[string]any{"name": textAt(pod, "metadata", "name"), "namespace": textAt(pod, "metadata", "namespace"), "cpuMillicores": cpu, "memoryBytes": memory, "timestamp": textAt(pod, "timestamp")})
	}
	return map[string]any{"rows": rows}, nil
}
