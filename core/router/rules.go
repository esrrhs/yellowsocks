package router

import (
	"strings"
	"sync"
)

// Rule is one domain routing policy.
// Action is "direct" or "proxy". A leading dot on a domain is a suffix, such as .cn.
type Rule struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Action  string `json:"action"`
	Builtin bool   `json:"builtin,omitempty"`
}

// NormalizeRules drops empty rows and keeps one row per kind+value. The later row wins.
func NormalizeRules(in []Rule) []Rule {
	seen := map[string]int{}
	var out []Rule
	for _, rule := range in {
		rule.Kind = strings.ToLower(strings.TrimSpace(rule.Kind))
		if rule.Kind != "domain" {
			continue
		}
		rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
		if rule.Action != "proxy" {
			rule.Action = "direct"
		}
		rule.Value = strings.TrimSpace(rule.Value)
		if rule.Kind == "domain" {
			raw := strings.ToLower(rule.Value)
			if strings.HasPrefix(raw, ".") {
				rule.Value = "." + strings.Trim(raw, ".")
			} else {
				rule.Value = strings.Trim(raw, ".")
			}
		}
		if rule.Value == "" || rule.Value == "." {
			continue
		}
		key := rule.Kind + "\n" + strings.ToLower(rule.Value)
		if idx, ok := seen[key]; ok {
			if rule.ID == "" {
				rule.ID = out[idx].ID
			}
			rule.Builtin = out[idx].Builtin || rule.Builtin
			out[idx] = rule
			continue
		}
		if rule.ID == "" {
			rule.ID = rule.Kind + "-" + strings.NewReplacer(" ", "-", "/", "-").Replace(rule.Value)
		}
		seen[key] = len(out)
		out = append(out, rule)
	}
	return out
}

// SetPolicy replaces domain rules. Private-network CIDRs stay in place.
func (r *Router) SetPolicy(rules []Rule) {
	clearMap(&r.directHosts)
	clearMap(&r.proxyHosts)
	for _, rule := range NormalizeRules(rules) {
		if rule.Action == "proxy" {
			r.AddProxyDomain(rule.Value)
		} else {
			r.AddDirectDomain(rule.Value)
		}
	}
}

func clearMap(m *sync.Map) {
	m.Range(func(key, _ any) bool {
		m.Delete(key)
		return true
	})
}
