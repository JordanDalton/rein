// Package policy applies the first local Rein Control policy slice.
package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/spec"
)

// Check blocks registered agents from mutating production Kubernetes. Humans
// still flow through Rein's existing approval gate.
func Check(caller string, argv []string, level risk.Level) error {
	return CheckIntent(caller, "", argv, level)
}

func CheckIntent(caller, intent string, argv []string, level risk.Level) error {
	var bundle struct {
		Rules []struct {
			Effect      string `json:"effect"`
			Caller      string `json:"caller"`
			Tool        string `json:"tool"`
			Command     string `json:"command"`
			Environment string `json:"environment"`
			Access      string `json:"access"`
		} `json:"rules"`
	}
	if b, err := os.ReadFile(filepath.Join(spec.Home(), "policy.json")); err == nil {
		_ = json.Unmarshal(b, &bundle)
	}
	for _, rule := range bundle.Rules {
		if !matches(rule.Caller, rule.Tool, rule.Command, rule.Environment, rule.Access, caller, intent, argv, level) {
			continue
		}
		switch rule.Effect {
		case "deny":
			return fmt.Errorf("%s is not permitted to perform this %s operation with %s", caller, rule.Access, rule.Tool)
		case "allow", "require_approval", "approval_required":
			return nil
		}
	}
	return nil
}

// RequiresApproval reports whether an organization rule requires a human
// approval for this operation. Unlike the caller's approval ceiling, this is a
// policy requirement and cannot be bypassed with --yes.
func RequiresApproval(caller, intent string, argv []string, level risk.Level) bool {
	b, err := os.ReadFile(filepath.Join(spec.Home(), "policy.json"))
	if err != nil {
		return false
	}
	var bundle struct {
		Rules []struct {
			Effect      string `json:"effect"`
			Caller      string `json:"caller"`
			Tool        string `json:"tool"`
			Command     string `json:"command"`
			Environment string `json:"environment"`
			Access      string `json:"access"`
		} `json:"rules"`
	}
	if json.Unmarshal(b, &bundle) != nil {
		return false
	}
	for _, rule := range bundle.Rules {
		if !matches(rule.Caller, rule.Tool, rule.Command, rule.Environment, rule.Access, caller, intent, argv, level) {
			continue
		}
		return rule.Effect == "require_approval" || rule.Effect == "approval_required"
	}
	return false
}

func matches(ruleCaller, ruleTool, ruleCommand, environment, access, caller, intent string, argv []string, level risk.Level) bool {
	if len(argv) == 0 {
		return false
	}
	command := strings.Join(argv[1:], " ")
	return wildcardMatch(ruleCaller, caller) && wildcardMatch(ruleTool, filepath.Base(argv[0])) &&
		wildcardMatch(ruleCommand, command) &&
		(environment == "" || strings.Contains(strings.ToLower(intent+" "+strings.Join(argv, " ")), strings.ToLower(environment))) &&
		(access == "any" || access == "" || access == risk.Access(level))
}

func wildcardMatch(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	matched, err := path.Match(pattern, value)
	return matched || (err != nil && pattern == value)
}

func production(argv []string) bool {
	for i, a := range argv {
		if a == "--namespace=production" || a == "--namespace=prod" || a == "-n=production" {
			return true
		}
		if (a == "-n" || a == "--namespace") && i+1 < len(argv) && (argv[i+1] == "production" || argv[i+1] == "prod") {
			return true
		}
	}
	return strings.Contains(strings.Join(argv, " "), "production")
}
