package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"

	dbx "github.com/t8y2/dbx/plugins/sdk/go/dbx-plugin-sdk"
)

// kindPermission answers "what may this credential do" for one browsable kind.
type kindPermission struct {
	List   bool `json:"list"`
	Mutate bool `json:"mutate"`
	Log    bool `json:"log,omitempty"`
}

type permissionReport struct {
	Namespace string                    `json:"namespace"`
	Identity  map[string]any            `json:"identity,omitempty"`
	Kinds     map[string]kindPermission `json:"kinds"`
	Cluster   map[string]bool           `json:"cluster"`
	Rules     []resourceRule            `json:"rules,omitempty"`
	Warnings  []string                  `json:"warnings,omitempty"`
}

type resourceRule struct {
	Verbs         []string `json:"verbs"`
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
}

// handlePermissions is what makes a namespace-scoped kubeconfig usable: the UI
// greys out every kind and every button the credential cannot use, instead of
// letting the user run into 403s one by one.
func (p *plugin) handlePermissions(params map[string]any) (any, *dbx.PluginError) {
	sess, perr := p.session(params)
	if perr != nil {
		return nil, perr
	}
	client, cerr := sess.kube()
	if cerr != nil {
		return nil, cerr
	}
	namespace := firstNonEmpty(stringValue(params, "namespace"), sess.settings.Namespace)
	ctx, cancel := context.WithTimeout(context.Background(), sess.settings.Timeout*3)
	defer cancel()

	report := permissionReport{
		Namespace: namespace,
		Kinds:     map[string]kindPermission{},
		Cluster:   map[string]bool{},
	}

	if identity, err := selfSubjectReview(ctx, client); err == nil {
		report.Identity = identity
	}

	rules, rulesErr := selfSubjectRules(ctx, client, namespace)
	switch {
	case rulesErr != nil:
		report.Warnings = append(report.Warnings, "无法读取命名空间权限规则（SelfSubjectRulesReview）："+rulesErr.Error())
	default:
		report.Rules = rules
	}

	// Namespaced kinds come from the rules review, which is namespace-scoped.
	for kind, info := range kindCatalog {
		if !info.Namespaced {
			continue
		}
		permission := kindPermission{
			List:   ruleAllows(rules, info.Group, info.Resource, "list"),
			Mutate: ruleMutates(rules, info.Group, info.Resource),
		}
		if kind == "pods" {
			permission.Log = ruleAllows(rules, "", "pods/log", "get")
		}
		report.Kinds[kind] = permission
	}

	// Cluster-scoped questions cannot be answered by a namespaced rules review,
	// so ask the API server directly.
	report.Cluster["namespaces"] = selfSubjectAccess(ctx, client, "", "namespaces", "list", "")
	report.Cluster["nodes"] = selfSubjectAccess(ctx, client, "", "nodes", "list", "")
	report.Cluster["storageclasses"] = selfSubjectAccess(ctx, client, "storage.k8s.io", "storageclasses", "list", "")
	// No namespace means "every namespace" for a namespaced resource: exactly
	// what the "all namespaces" option needs.
	report.Cluster["allNamespaces"] = selfSubjectAccess(ctx, client, "", "pods", "list", "")
	report.Cluster["clusterScopedList"] = selfSubjectAccess(ctx, client, "", "events", "list", "")

	report.Kinds["nodes"] = kindPermission{List: report.Cluster["nodes"]}
	report.Kinds["namespaces"] = kindPermission{List: report.Cluster["namespaces"]}
	report.Kinds["storageclasses"] = kindPermission{List: report.Cluster["storageclasses"]}

	return report, nil
}

func ruleAllows(rules []resourceRule, group, resource, verb string) bool {
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 {
			// A rule limited to named objects never grants collection access.
			continue
		}
		if !containsString(rule.Verbs, verb) && !containsString(rule.Verbs, "*") {
			continue
		}
		if !groupMatches(rule.APIGroups, group) {
			continue
		}
		if resourceMatches(rule.Resources, resource) {
			return true
		}
	}
	return false
}

func ruleMutates(rules []resourceRule, group, resource string) bool {
	for _, verb := range []string{"patch", "update", "delete", "deletecollection", "*"} {
		if ruleAllows(rules, group, resource, verb) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func groupMatches(groups []string, group string) bool {
	for _, value := range groups {
		if value == "*" || value == group {
			return true
		}
	}
	return false
}

func resourceMatches(resources []string, resource string) bool {
	for _, value := range resources {
		if value == "*" || value == resource || value == resource+"/*" {
			return true
		}
	}
	return false
}

func selfSubjectRules(ctx context.Context, client *kubeClient, namespace string) ([]resourceRule, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectRulesReview",
		"spec":       map[string]any{"namespace": namespace},
	})
	data, err := client.request(ctx, http.MethodPost,
		"/apis/authorization.k8s.io/v1/selfsubjectrulesreviews", body, "application/json")
	if err != nil {
		return nil, err
	}
	var out struct {
		Status struct {
			ResourceRules []resourceRule `json:"resourceRules"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Status.ResourceRules, nil
}

func selfSubjectReview(ctx context.Context, client *kubeClient) (map[string]any, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "SelfSubjectReview",
	})
	data, err := client.request(ctx, http.MethodPost,
		"/apis/authentication.k8s.io/v1/selfsubjectreviews", body, "application/json")
	if err != nil {
		return nil, err
	}
	var out struct {
		Status struct {
			UserInfo map[string]any `json:"userInfo"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Status.UserInfo, nil
}

func selfSubjectAccess(ctx context.Context, client *kubeClient, group, resource, verb, namespace string) bool {
	attributes := map[string]any{"verb": verb, "resource": resource}
	if group != "" {
		attributes["group"] = group
	}
	if namespace != "" {
		attributes["namespace"] = namespace
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec":       map[string]any{"resourceAttributes": attributes},
	})
	data, err := client.request(ctx, http.MethodPost,
		"/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", body, "application/json")
	if err != nil {
		return false
	}
	var out struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return false
	}
	return out.Status.Allowed
}

// summarizePermissions turns the rule list into a short human string used by the
// workbench permission panel.
func summarizePermissions(report permissionReport) string {
	if len(report.Rules) == 0 {
		return "没有可用的命名空间规则"
	}
	lines := make([]string, 0, len(report.Rules))
	for _, rule := range report.Rules {
		groups := "*"
		if len(rule.APIGroups) > 0 {
			groups = joinStrings(rule.APIGroups)
		}
		lines = append(lines, fmt.Sprintf("%s %s → %s", joinStrings(rule.Verbs), groups, joinStrings(rule.Resources)))
	}
	sort.Strings(lines)
	return joinStrings(lines)
}

func joinStrings(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += ","
		}
		out += value
	}
	return out
}
