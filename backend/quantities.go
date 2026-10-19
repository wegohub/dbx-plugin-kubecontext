package main

import (
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Kubernetes quantities, normalized only for display/plotting. Raw API values
// remain authoritative; no quantities calculated here are ever written back.
var quantityPattern = regexp.MustCompile(`^([+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)([a-zA-Z]*)$`)

func quantity(value string) *float64 {
	m := quantityPattern.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return nil
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil || n < 0 {
		return nil
	}
	factors := map[string]float64{"": 1, "n": 1e-9, "u": 1e-6, "m": 1e-3, "k": 1e3, "K": 1e3, "M": 1e6, "G": 1e9, "T": 1e12, "P": 1e15, "E": 1e18, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60}
	factor, ok := factors[m[2]]
	if !ok {
		return nil
	}
	n *= factor
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func multiplied(value *float64, factor float64) *float64 {
	if value == nil {
		return nil
	}
	n := *value * factor
	return &n
}
func formatCPU(value *float64) string {
	if value == nil {
		return "未设置"
	}
	if *value >= 1000 {
		return strconv.FormatFloat(*value/1000, 'f', -1, 64) + " 核"
	}
	return strconv.FormatFloat(math.Round(*value*1000)/1000, 'f', -1, 64) + "m"
}
func formatMemory(value *float64) string {
	if value == nil {
		return "未设置"
	}
	for _, unit := range []struct {
		name   string
		factor float64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if *value >= unit.factor {
			return strconv.FormatFloat(math.Round(*value/unit.factor*100)/100, 'f', -1, 64) + " " + unit.name
		}
	}
	return strconv.FormatFloat(*value, 'f', -1, 64) + " B"
}

type containerBudget struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	CPURequest    *float64 `json:"cpuRequestMilli"`
	CPULimit      *float64 `json:"cpuLimitMilli"`
	MemoryRequest *float64 `json:"memoryRequestBytes"`
	MemoryLimit   *float64 `json:"memoryLimitBytes"`
}
type podBudget struct {
	CPURequest         *float64          `json:"cpuRequestMilli"`
	CPULimit           *float64          `json:"cpuLimitMilli"`
	MemoryRequest      *float64          `json:"memoryRequestBytes"`
	MemoryLimit        *float64          `json:"memoryLimitBytes"`
	CPULimitPartial    bool              `json:"cpuLimitPartial"`
	MemoryLimitPartial bool              `json:"memoryLimitPartial"`
	Containers         []containerBudget `json:"containers"`
}

func effectiveResource(object map[string]any, kind, resource string) (*float64, bool) {
	if value := quantity(textAt(object, "spec", "resources", kind, resource)); value != nil {
		if overhead := quantity(textAt(object, "spec", "overhead", resource)); overhead != nil {
			*value += *overhead
		}
		return value, false
	}
	runtime, peak, sidecars := 0.0, 0.0, 0.0
	anySet, missing := false, false
	add := func(container map[string]any) float64 {
		value := quantity(textAt(container, "resources", kind, resource))
		if value == nil {
			if kind == "limits" {
				missing = true
			}
			return 0
		}
		anySet = true
		return *value
	}
	for _, container := range mapListAt(object, "spec", "containers") {
		runtime += add(container)
	}
	for _, container := range mapListAt(object, "spec", "initContainers") {
		amount := add(container)
		candidate := sidecars + amount
		if textAt(container, "restartPolicy") == "Always" {
			sidecars += amount
			runtime += amount
			candidate = sidecars
		}
		peak = math.Max(peak, candidate)
	}
	if kind == "limits" && missing {
		return nil, anySet
	}
	value := math.Max(runtime, peak)
	if overhead := quantity(textAt(object, "spec", "overhead", resource)); overhead != nil && (kind == "requests" || anySet) {
		value += *overhead
		anySet = true
	}
	if !anySet {
		return nil, false
	}
	return &value, false
}
func resourceBudget(object map[string]any) podBudget {
	b := podBudget{Containers: []containerBudget{}}
	b.CPURequest, _ = effectiveResource(object, "requests", "cpu")
	b.CPURequest = multiplied(b.CPURequest, 1000)
	b.CPULimit, b.CPULimitPartial = effectiveResource(object, "limits", "cpu")
	b.CPULimit = multiplied(b.CPULimit, 1000)
	b.MemoryRequest, _ = effectiveResource(object, "requests", "memory")
	b.MemoryLimit, b.MemoryLimitPartial = effectiveResource(object, "limits", "memory")
	for _, section := range []string{"containers", "initContainers"} {
		for _, container := range mapListAt(object, "spec", section) {
			kind := "container"
			if section == "initContainers" {
				kind = "init"
				if textAt(container, "restartPolicy") == "Always" {
					kind = "sidecar"
				}
			}
			b.Containers = append(b.Containers, containerBudget{Name: textAt(container, "name"), Kind: kind,
				CPURequest: multiplied(quantity(textAt(container, "resources", "requests", "cpu")), 1000), CPULimit: multiplied(quantity(textAt(container, "resources", "limits", "cpu")), 1000), MemoryRequest: quantity(textAt(container, "resources", "requests", "memory")), MemoryLimit: quantity(textAt(container, "resources", "limits", "memory"))})
		}
	}
	return b
}
