package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

type kindInfo struct {
	Group       string
	Version     string
	Resource    string
	Namespaced  bool
	Label       string
	Scalable    bool
	Restartable bool
}

// kindCatalog is the browsable surface of the workbench. Extend it (or switch to
// client-go discovery) when you need CRDs.
var kindCatalog = map[string]kindInfo{
	"pods":                   {"", "v1", "pods", true, "Pods", false, false},
	"deployments":            {"apps", "v1", "deployments", true, "Deployments", true, true},
	"statefulsets":           {"apps", "v1", "statefulsets", true, "StatefulSets", true, true},
	"daemonsets":             {"apps", "v1", "daemonsets", true, "DaemonSets", false, true},
	"replicasets":            {"apps", "v1", "replicasets", true, "ReplicaSets", true, false},
	"jobs":                   {"batch", "v1", "jobs", true, "Jobs", false, false},
	"cronjobs":               {"batch", "v1", "cronjobs", true, "CronJobs", false, false},
	"services":               {"", "v1", "services", true, "Services", false, false},
	"ingresses":              {"networking.k8s.io", "v1", "ingresses", true, "Ingresses", false, false},
	"configmaps":             {"", "v1", "configmaps", true, "ConfigMaps", false, false},
	"secrets":                {"", "v1", "secrets", true, "Secrets", false, false},
	"serviceaccounts":        {"", "v1", "serviceaccounts", true, "ServiceAccounts", false, false},
	"persistentvolumeclaims": {"", "v1", "persistentvolumeclaims", true, "PVCs", false, false},
	"events":                 {"", "v1", "events", true, "Events", false, false},
	"nodes":                  {"", "v1", "nodes", false, "Nodes", false, false},
	"namespaces":             {"", "v1", "namespaces", false, "Namespaces", false, false},
	"storageclasses":         {"storage.k8s.io", "v1", "storageclasses", false, "StorageClasses", false, false},
}

func (k kindInfo) prefix() string {
	if k.Group == "" {
		return "/api/" + k.Version
	}
	return "/apis/" + k.Group + "/" + k.Version
}

func (k kindInfo) collectionPath(namespace string) string {
	if k.Namespaced && namespace != "" {
		return k.prefix() + "/namespaces/" + url.PathEscape(namespace) + "/" + k.Resource
	}
	return k.prefix() + "/" + k.Resource
}

func (k kindInfo) objectPath(namespace, name string) string {
	return k.collectionPath(namespace) + "/" + url.PathEscape(name)
}

type resourceRow struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Status    string            `json:"status"`
	Ready     string            `json:"ready,omitempty"`
	Detail    string            `json:"detail,omitempty"`
	Age       string            `json:"age,omitempty"`
	CreatedAt string            `json:"createdAt,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
}

func dig(value any, keys ...string) any {
	current := value
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}

func textAt(value any, keys ...string) string {
	switch v := dig(value, keys...).(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	}
	return ""
}

func numAt(value any, keys ...string) float64 {
	switch v := dig(value, keys...).(type) {
	case float64:
		return v
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n
		}
	}
	return 0
}

func listAt(value any, keys ...string) []any {
	if items, ok := dig(value, keys...).([]any); ok {
		return items
	}
	return nil
}

func mapListAt(value any, keys ...string) []map[string]any {
	items := listAt(value, keys...)
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func ageString(created string) string {
	if created == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, created)
	if err != nil {
		return ""
	}
	delta := time.Since(parsed)
	switch {
	case delta < time.Minute:
		return fmt.Sprintf("%ds", int(delta.Seconds()))
	case delta < time.Hour:
		return fmt.Sprintf("%dm", int(delta.Minutes()))
	case delta < 24*time.Hour:
		return fmt.Sprintf("%dh", int(delta.Hours()))
	default:
		return fmt.Sprintf("%dd", int(delta.Hours()/24))
	}
}

func firstContainerImage(object map[string]any) string {
	for _, container := range mapListAt(object, "spec", "containers") {
		return textAt(container, "image")
	}
	return ""
}

func summarize(kind string, object map[string]any) resourceRow {
	row := resourceRow{
		Name:      textAt(object, "metadata", "name"),
		Namespace: textAt(object, "metadata", "namespace"),
		Age:       ageString(textAt(object, "metadata", "creationTimestamp")),
		CreatedAt: textAt(object, "metadata", "creationTimestamp"),
		Fields:    resourceFields(kind, object),
	}
	switch kind {
	case "pods":
		row.Status, row.Ready = podStatus(object)
		row.Detail = row.Fields["node"]
	case "deployments", "statefulsets", "replicasets":
		desired := int(numAt(object, "spec", "replicas"))
		ready := int(numAt(object, "status", "readyReplicas"))
		row.Ready = fmt.Sprintf("%d/%d", ready, desired)
		row.Status = "Ready"
		if ready < desired {
			row.Status = "Progressing"
		}
		row.Detail = firstContainerImage(object)
	case "daemonsets":
		desired := int(numAt(object, "status", "desiredNumberScheduled"))
		ready := int(numAt(object, "status", "numberReady"))
		row.Ready = fmt.Sprintf("%d/%d", ready, desired)
		row.Status = "Ready"
		if ready < desired {
			row.Status = "Progressing"
		}
		row.Detail = firstContainerImage(object)
	case "jobs":
		row.Status = "Running"
		if numAt(object, "status", "succeeded") > 0 {
			row.Status = "Complete"
		} else if numAt(object, "status", "failed") > 0 {
			row.Status = "Failed"
		}
		row.Ready = fmt.Sprintf("%d/%d", int(numAt(object, "status", "succeeded")), int(numAt(object, "spec", "completions")))
	case "cronjobs":
		row.Status = "Scheduled"
		if suspended := dig(object, "spec", "suspend"); isTrue(suspended) {
			row.Status = "Suspended"
		}
		row.Detail = textAt(object, "spec", "schedule")
	case "services":
		row.Status = textAt(object, "spec", "type")
		row.Detail = firstNonEmpty(textAt(object, "spec", "clusterIP"), "-")
		if ports := mapListAt(object, "spec", "ports"); len(ports) > 0 {
			row.Ready = fmt.Sprintf("%d port(s)", len(ports))
		}
	case "ingresses":
		hosts := []string{}
		for _, rule := range mapListAt(object, "spec", "rules") {
			if host := textAt(rule, "host"); host != "" {
				hosts = append(hosts, host)
			}
		}
		row.Status = "Active"
		row.Detail = strings.Join(hosts, ", ")
	case "configmaps", "secrets":
		data := dig(object, "data")
		keys := []string{}
		if m, ok := data.(map[string]any); ok {
			for key := range m {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		row.Status = textAt(object, "type")
		row.Ready = fmt.Sprintf("%d key(s)", len(keys))
		if len(keys) > 6 {
			keys = keys[:6]
		}
		row.Detail = strings.Join(keys, ", ")
	case "serviceaccounts":
		row.Status = "Active"
		row.Ready = fmt.Sprintf("%d secret(s)", len(listAt(object, "secrets")))
	case "persistentvolumeclaims":
		row.Status = textAt(object, "status", "phase")
		row.Ready = textAt(object, "status", "capacity", "storage")
		row.Detail = textAt(object, "spec", "storageClassName")
	case "storageclasses":
		row.Status = textAt(object, "provisioner")
		row.Detail = textAt(object, "reclaimPolicy")
	case "nodes":
		row.Status = "NotReady"
		for _, condition := range mapListAt(object, "status", "conditions") {
			if textAt(condition, "type") == "Ready" && textAt(condition, "status") == "True" {
				row.Status = "Ready"
			}
		}
		row.Detail = textAt(object, "status", "nodeInfo", "kubeletVersion")
		if roles := nodeRoles(object); roles != "" {
			row.Ready = roles
		}
	case "namespaces":
		row.Status = textAt(object, "status", "phase")
	case "events":
		row.Status = textAt(object, "type")
		row.Ready = textAt(object, "count")
		row.Detail = fmt.Sprintf("%s · %s", textAt(object, "reason"), truncate(textAt(object, "message"), 120))
		if row.Age == "" {
			row.Age = ageString(textAt(object, "lastTimestamp"))
		}
	default:
		row.Status = "-"
	}
	return row
}

func nodeRoles(object map[string]any) string {
	labels, ok := dig(object, "metadata", "labels").(map[string]any)
	if !ok {
		return ""
	}
	roles := []string{}
	for key := range labels {
		if strings.HasPrefix(key, "node-role.kubernetes.io/") {
			role := strings.TrimPrefix(key, "node-role.kubernetes.io/")
			if role != "" {
				roles = append(roles, role)
			}
		}
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

func isTrue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true")
	}
	return false
}

func nonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func truncate(value string, limit int) string {
	if len([]rune(value)) <= limit {
		return value
	}
	return string([]rune(value)[:limit]) + "…"
}

func readFileString(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func toPluginError(err error) *dbx.PluginError {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusUnauthorized:
			return dbx.NewError(-32002, "认证失败（401）：token 或客户端证书无效")
		case http.StatusForbidden:
			return dbx.NewError(-32003, "权限不足（403）："+firstNonEmpty(apiErr.Message, "该 ServiceAccount/用户没有这个动词的 RBAC 权限"))
		case http.StatusNotFound:
			return dbx.NewError(-32004, "对象不存在（404）："+apiErr.Message)
		}
	}
	return dbx.NewError(-32603, err.Error())
}

func (p *plugin) handleList(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	kind := firstNonEmpty(stringValue(params, "kind"), "pods")
	info, ok := kindCatalog[kind]
	if !ok {
		return nil, dbx.NewError(-32602, "不支持的资源类型："+kind)
	}
	namespace := stringValue(params, "namespace")
	if namespace == "" {
		namespace = sess.settings.Namespace
	}
	query := url.Values{}
	query.Set("limit", "500")
	if selector := stringValue(params, "labelSelector"); selector != "" {
		query.Set("labelSelector", selector)
	}
	path := info.collectionPath(namespace)
	if info.Namespaced && namespace == "*" {
		path = info.prefix() + "/" + info.Resource
	}
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*2)
	defer cancel()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.getJSON(ctx, path+"?"+query.Encode(), &list); err != nil {
		return nil, toPluginError(err)
	}
	rows := make([]resourceRow, 0, len(list.Items))
	for _, item := range list.Items {
		rows = append(rows, summarize(kind, item))
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Namespace != rows[j].Namespace {
			return rows[i].Namespace < rows[j].Namespace
		}
		return rows[i].Name < rows[j].Name
	})
	return map[string]any{
		"kind":      kind,
		"label":     info.Label,
		"namespace": namespace,
		"rows":      rows,
	}, nil
}

func (p *plugin) handleGet(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	info, err := requireKind(stringValue(params, "kind"))
	if err != nil {
		return nil, err
	}
	name := stringValue(params, "name")
	if name == "" {
		return nil, dbx.NewError(-32602, "缺少对象名称")
	}
	namespace := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*2)
	defer cancel()
	var object map[string]any
	if getErr := client.getJSON(ctx, info.objectPath(namespace, name), &object); getErr != nil {
		return nil, toPluginError(getErr)
	}
	pretty, marshalErr := json.MarshalIndent(object, "", "  ")
	if marshalErr != nil {
		return nil, dbx.NewError(-32603, marshalErr.Error())
	}
	return map[string]any{"json": string(pretty), "row": summarize(stringValue(params, "kind"), object)}, nil
}

func (p *plugin) handleLogs(params map[string]any) (any, *dbx.PluginError) {
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
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*4)
	defer cancel()

	info := kindCatalog["pods"]
	var object map[string]any
	if err := client.getJSON(ctx, info.objectPath(namespace, pod), &object); err != nil {
		return nil, toPluginError(err)
	}
	containers := []string{}
	for _, container := range mapListAt(object, "spec", "containers") {
		if name := textAt(container, "name"); name != "" {
			containers = append(containers, name)
		}
	}
	container := stringValue(params, "container")
	if container == "" && len(containers) > 0 {
		container = containers[0]
	}
	query := url.Values{}
	query.Set("tailLines", strconv.Itoa(clampInt(intValue(params, "tailLines", 500), 1, 5000)))
	if container != "" {
		query.Set("container", container)
	}
	if boolValue(params, "previous") {
		query.Set("previous", "true")
	}
	logs, err := client.getText(ctx, info.objectPath(namespace, pod)+"/log?"+query.Encode())
	if err != nil {
		return nil, toPluginError(err)
	}
	return map[string]any{
		"logs":       stripANSI(logs),
		"container":  container,
		"containers": containers,
		"pod":        pod,
		"namespace":  namespace,
	}, nil
}

func (p *plugin) handleRelatedEvents(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	namespace := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	name := stringValue(params, "name")
	if name == "" {
		return nil, dbx.NewError(-32602, "缺少对象名称")
	}
	query := url.Values{}
	query.Set("fieldSelector", "involvedObject.name="+name)
	query.Set("limit", "100")
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*2)
	defer cancel()
	var list struct {
		Items []map[string]any `json:"items"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/events"
	if err := client.getJSON(ctx, path+"?"+query.Encode(), &list); err != nil {
		return nil, toPluginError(err)
	}
	rows := make([]resourceRow, 0, len(list.Items))
	for _, item := range list.Items {
		rows = append(rows, summarize("events", item))
	}
	return map[string]any{"rows": rows}, nil
}

func (p *plugin) handleOverview(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*2)
	defer cancel()

	result := map[string]any{
		"connectionId": sess.settings.ConnectionID,
		"name":         sess.settings.Name,
		"context":      sess.settings.Context,
		"namespace":    sess.settings.Namespace,
		"readOnly":     true,
	}

	version, versionErr := client.serverVersion(ctx)
	if versionErr != nil {
		return nil, toPluginError(versionErr)
	}
	result["version"] = version

	var namespaces struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.getJSON(ctx, "/api/v1/namespaces?limit=200", &namespaces); err == nil {
		names := make([]string, 0, len(namespaces.Items))
		for _, item := range namespaces.Items {
			names = append(names, textAt(item, "metadata", "name"))
		}
		sort.Strings(names)
		result["namespaces"] = names
	} else {
		// A namespace-scoped ServiceAccount cannot list namespaces; fall back to
		// the connection default instead of failing the whole overview.
		result["namespaces"] = []string{sess.settings.Namespace}
	}

	var nodes struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.getJSON(ctx, "/api/v1/nodes?limit=500", &nodes); err == nil {
		ready := 0
		for _, item := range nodes.Items {
			if summarize("nodes", item).Status == "Ready" {
				ready++
			}
		}
		result["nodes"] = map[string]any{"total": len(nodes.Items), "ready": ready}
	}
	return result, nil
}

func requireKind(kind string) (kindInfo, *dbx.PluginError) {
	if kind == "" {
		return kindInfo{}, dbx.NewError(-32602, "缺少资源类型")
	}
	info, ok := kindCatalog[kind]
	if !ok {
		return kindInfo{}, dbx.NewError(-32602, "不支持的资源类型："+kind)
	}
	return info, nil
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
