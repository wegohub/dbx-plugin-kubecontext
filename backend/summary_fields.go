package main

import (
	"fmt"
	"sort"
	"strings"
)

func labelText(value any) string {
	labels, _ := value.(map[string]any)
	parts := make([]string, 0, len(labels))
	for key, value := range labels {
		parts = append(parts, fmt.Sprintf("%s=%v", key, value))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func containerImages(object map[string]any, path ...string) string {
	images := []string{}
	seen := map[string]bool{}
	for _, container := range mapListAt(object, path...) {
		image := textAt(container, "image")
		if image != "" && !seen[image] {
			seen[image] = true
			images = append(images, image)
		}
	}
	return strings.Join(images, ", ")
}

func podStatus(object map[string]any) (string, string) {
	statuses := mapListAt(object, "status", "containerStatuses")
	total := len(mapListAt(object, "spec", "containers"))
	if total == 0 {
		total = len(statuses)
	}
	ready := 0
	for _, status := range statuses {
		if isTrue(status["ready"]) {
			ready++
		}
	}
	result := firstNonEmpty(textAt(object, "status", "reason"), textAt(object, "status", "phase"), "Unknown")
	for _, status := range statuses {
		if reason := textAt(status, "state", "waiting", "reason"); reason != "" {
			result = reason
			break
		}
		if numAt(status, "state", "terminated", "exitCode") != 0 {
			result = firstNonEmpty(textAt(status, "state", "terminated", "reason"), "Error")
		}
	}
	for _, status := range mapListAt(object, "status", "initContainerStatuses") {
		if reason := textAt(status, "state", "waiting", "reason"); reason != "" {
			result = "Init:" + reason
			break
		}
		if numAt(status, "state", "terminated", "exitCode") != 0 {
			result = "Init:" + firstNonEmpty(textAt(status, "state", "terminated", "reason"), "Error")
			break
		}
	}
	if textAt(object, "metadata", "deletionTimestamp") != "" {
		result = "Terminating"
	}
	return result, fmt.Sprintf("%d/%d", ready, total)
}

func resourceFields(kind string, object map[string]any) map[string]string {
	fields := map[string]string{}
	switch kind {
	case "pods":
		budget := resourceBudget(object)
		restarts := 0
		statuses := append(mapListAt(object, "status", "containerStatuses"), mapListAt(object, "status", "initContainerStatuses")...)
		reason := textAt(object, "status", "message")
		for _, status := range statuses {
			restarts += int(numAt(status, "restartCount"))
			reason = firstNonEmpty(reason, textAt(status, "state", "waiting", "message"), textAt(status, "state", "terminated", "message"))
		}
		owner := ""
		for _, ref := range mapListAt(object, "metadata", "ownerReferences") {
			if owner == "" || isTrue(ref["controller"]) {
				owner = textAt(ref, "kind") + "/" + textAt(ref, "name")
			}
			if isTrue(ref["controller"]) {
				break
			}
		}
		fields = map[string]string{
			"restarts": fmt.Sprint(restarts), "node": textAt(object, "spec", "nodeName"),
			"podIP": textAt(object, "status", "podIP"), "hostIP": textAt(object, "status", "hostIP"),
			"images": containerImages(object, "spec", "containers"), "owner": owner,
			"qos": textAt(object, "status", "qosClass"), "serviceAccount": textAt(object, "spec", "serviceAccountName"),
			"reason": reason, "labels": labelText(dig(object, "metadata", "labels")),
		}
		fields["cpuRequest"] = formatCPU(budget.CPURequest)
		fields["cpuLimit"] = formatCPU(budget.CPULimit)
		fields["memoryRequest"] = formatMemory(budget.MemoryRequest)
		fields["memoryLimit"] = formatMemory(budget.MemoryLimit)
		if budget.CPULimitPartial {
			fields["cpuLimit"] = "部分未设置"
		}
		if budget.MemoryLimitPartial {
			fields["memoryLimit"] = "部分未设置"
		}
	case "deployments", "statefulsets", "replicasets", "daemonsets":
		fields["images"] = containerImages(object, "spec", "template", "spec", "containers")
		fields["selector"] = labelText(dig(object, "spec", "selector", "matchLabels"))
		fields["updated"] = textAt(object, "status", "updatedReplicas")
		if kind == "daemonsets" {
			fields["updated"] = textAt(object, "status", "updatedNumberScheduled")
		}
		fields["available"] = textAt(object, "status", "availableReplicas")
		if kind == "daemonsets" {
			fields["available"] = textAt(object, "status", "numberAvailable")
		}
	case "services":
		fields["clusterIP"] = textAt(object, "spec", "clusterIP")
		fields["selector"] = labelText(dig(object, "spec", "selector"))
		ports := []string{}
		for _, port := range mapListAt(object, "spec", "ports") {
			value := textAt(port, "port")
			if node := textAt(port, "nodePort"); node != "" {
				value += ":" + node
			}
			value += "/" + firstNonEmpty(textAt(port, "protocol"), "TCP")
			ports = append(ports, value)
		}
		fields["ports"] = strings.Join(ports, ", ")
		addresses := []string{}
		for _, address := range listAt(object, "spec", "externalIPs") {
			if text, ok := address.(string); ok {
				addresses = append(addresses, text)
			}
		}
		for _, entry := range mapListAt(object, "status", "loadBalancer", "ingress") {
			addresses = append(addresses, firstNonEmpty(textAt(entry, "ip"), textAt(entry, "hostname")))
		}
		if name := textAt(object, "spec", "externalName"); name != "" {
			addresses = append(addresses, name)
		}
		fields["externalIPs"] = strings.Join(nonEmpty(addresses), ", ")
	case "nodes":
		for _, address := range mapListAt(object, "status", "addresses") {
			switch textAt(address, "type") {
			case "InternalIP":
				fields["internalIP"] = textAt(address, "address")
			case "ExternalIP":
				fields["externalIP"] = textAt(address, "address")
			}
		}
		fields["cpu"] = textAt(object, "status", "allocatable", "cpu")
		fields["memory"] = textAt(object, "status", "allocatable", "memory")
		fields["os"] = textAt(object, "status", "nodeInfo", "osImage")
		fields["runtime"] = textAt(object, "status", "nodeInfo", "containerRuntimeVersion")
	case "persistentvolumeclaims":
		fields["volume"] = textAt(object, "spec", "volumeName")
		modes := []string{}
		for _, mode := range listAt(object, "spec", "accessModes") {
			if value, ok := mode.(string); ok {
				modes = append(modes, value)
			}
		}
		fields["accessModes"] = strings.Join(modes, ", ")
	}
	return fields
}
