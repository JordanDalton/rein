package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jordandalton/rein/internal/risk"
)

var ciClient = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
	return errors.New("CI authentication redirects are not allowed")
}}

type ciRule struct {
	Effect      string `json:"effect"`
	Caller      string `json:"caller"`
	Tool        string `json:"tool"`
	Command     string `json:"command"`
	Environment string `json:"environment"`
	Access      string `json:"access"`
}
type ciTime struct{ time.Time }

func (t *ciTime) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			t.Time = parsed
			return nil
		}
	}
	return errors.New("invalid policy expiration")
}

var ciExecute = func(command *exec.Cmd) error { return command.Run() }

type ciBundle struct {
	Version   int      `json:"version"`
	Rules     []ciRule `json:"rules"`
	ExpiresAt ciTime   `json:"expires_at"`
}
type ciSession struct{ Endpoint, Token, Caller string }

func (s ciSession) request(ctx context.Context, method, route string, body, result any) error {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, s.Endpoint+"/api/v1/rein/"+route, input)
	if err != nil {
		return errors.New("invalid CI request")
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	response, err := ciClient.Do(req)
	if err != nil {
		return errors.New("CI control plane request failed; refusing offline fallback")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("CI control plane returned HTTP %d", response.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 2*1024*1024)).Decode(result); err != nil {
			return errors.New("invalid CI control plane response")
		}
	}
	return nil
}

func ciDecision(bundle ciBundle, caller, environment string, argv []string) (string, error) {
	if bundle.Version < 1 || bundle.ExpiresAt.IsZero() || !bundle.ExpiresAt.After(time.Now()) {
		return "", errors.New("CI requires a published, unexpired policy bundle")
	}
	for _, rule := range bundle.Rules {
		if rule.Effect != "allow" && rule.Effect != "deny" && rule.Effect != "require_approval" && rule.Effect != "approval_required" {
			return "", errors.New("unsupported policy effect")
		}
		if rule.Access != "" && rule.Access != "any" && rule.Access != "write" {
			return "", errors.New("unsupported policy access condition")
		}
		for _, pattern := range []string{rule.Caller, rule.Tool, rule.Command} {
			if _, err := path.Match(pattern, ""); err != nil {
				return "", errors.New("invalid policy pattern")
			}
		}
	}
	if len(argv) == 0 {
		return "", errors.New("command is required")
	}
	matches := func(pattern, value string) bool {
		if pattern == "" {
			return true
		}
		yes, _ := path.Match(pattern, value)
		return yes
	}
	for _, rule := range bundle.Rules {
		if !matches(rule.Caller, caller) || !matches(rule.Tool, filepath.Base(argv[0])) || !matches(rule.Command, strings.Join(argv[1:], " ")) {
			continue
		}
		if rule.Environment != "" && rule.Environment != environment {
			continue
		}
		if rule.Access == "write" && risk.Classify(argv) == risk.Safe {
			continue
		}
		return rule.Effect, nil
	}
	// CI is intentionally stricter than the interactive default-allow policy.
	return "deny", nil
}

func cmdCI(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "check" && args[0] != "run") {
		return errors.New("usage: rein ci check | rein ci run [--environment NAME] [--approval-timeout 2m] [--timeout 10m] -- TOOL ARGS...; requires REIN_WORKLOAD_TOKEN")
	}
	fs := flagSet("ci")
	environment := fs.String("environment", "", "")
	approvalWait := fs.Duration("approval-timeout", 0, "")
	timeout := fs.Duration("timeout", 10*time.Minute, "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *approvalWait < 0 || *approvalWait > 10*time.Minute || *timeout <= 0 || *timeout > time.Hour {
		return errors.New("approval timeout must be 0..10m; command timeout must be >0..1h")
	}
	if args[0] == "check" && fs.NArg() != 0 {
		return errors.New("ci check does not take a command")
	}
	if args[0] == "run" && fs.NArg() == 0 {
		return errors.New("ci run requires -- TOOL ARGS...")
	}
	endpoint := cloudURL("")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("REIN_CONTROL_URL must be an HTTPS origin without credentials, query, or path")
	}
	token := os.Getenv("REIN_WORKLOAD_TOKEN")
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("set REIN_WORKLOAD_TOKEN from workload federation or a CI secret; browser login is not used")
	}
	s := ciSession{Endpoint: strings.TrimRight(endpoint, "/"), Token: token}
	var identity struct {
		Type     string `json:"credential_type"`
		Provider string `json:"provider"`
	}
	if err := s.request(ctx, http.MethodGet, "status", nil, &identity); err != nil {
		return err
	}
	if identity.Type != "workload" || identity.Provider == "" || len(identity.Provider) > 64 {
		return errors.New("CI requires a workload credential with a provider configured")
	}
	s.Caller = identity.Provider
	var bundle ciBundle
	if err := s.request(ctx, http.MethodGet, "policy-bundles/latest", nil, &bundle); err != nil {
		return err
	}
	if _, err := ciDecision(bundle, s.Caller, *environment, []string{"__ci_check__"}); err != nil {
		return err
	}
	if args[0] == "check" {
		fmt.Printf("Workload authenticated. Published policy %d available. No command executed.\n", bundle.Version)
		return nil
	}
	argv := fs.Args()
	effect, err := ciDecision(bundle, s.Caller, *environment, argv)
	if err != nil {
		return err
	}
	operation := map[string]any{"tool": filepath.Base(argv[0]), "argv": argv, "environment": *environment, "policy_version": bundle.Version}
	audit := func(event string, extra map[string]any) error {
		metadata := map[string]any{"github_run_id": os.Getenv("GITHUB_RUN_ID"), "github_job": os.Getenv("GITHUB_JOB")}
		for key, value := range extra {
			metadata[key] = value
		}
		return s.request(ctx, http.MethodPost, "audit-events", map[string]any{"event": event, "caller": s.Caller, "operation": operation, "metadata": metadata}, nil)
	}
	if effect == "deny" {
		if err := audit("execution.blocked", nil); err != nil {
			return err
		}
		return errors.New("CI operation denied by policy (an explicit matching rule is required)")
	}
	if effect == "require_approval" || effect == "approval_required" || risk.Classify(argv) != risk.Safe {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		canonical, _ := json.Marshal(map[string]any{"operation": operation, "nonce": hex.EncodeToString(nonce)})
		hash := fmt.Sprintf("%x", sha256.Sum256(canonical))
		var approval struct {
			ID string `json:"id"`
		}
		if err := s.request(ctx, http.MethodPost, "approvals", map[string]any{"caller": s.Caller, "operation": operation, "operation_hash": hash}, &approval); err != nil {
			return err
		}
		if approval.ID == "" {
			return errors.New("approval response missing ID")
		}
		fmt.Printf("Approval requested: %s. Command has not run.\n", approval.ID)
		if *approvalWait == 0 {
			return errors.New("approval required; use --approval-timeout to wait within this invocation")
		}
		waitCtx, cancel := context.WithTimeout(ctx, *approvalWait)
		defer cancel()
		for {
			var result struct {
				Approved bool `json:"approved"`
			}
			if err := s.request(waitCtx, http.MethodGet, "approvals/check?operation_hash="+hash+"&caller="+url.QueryEscape(s.Caller), nil, &result); err != nil {
				return err
			}
			if result.Approved {
				break
			}
			select {
			case <-waitCtx.Done():
				return errors.New("approval wait ended; no command executed")
			case <-time.After(2 * time.Second):
			}
		}
		// Reauthenticate and refresh after a human wait. A new policy requires a new invocation.
		var fresh ciBundle
		if err := s.request(ctx, http.MethodGet, "policy-bundles/latest", nil, &fresh); err != nil {
			return err
		}
		if fresh.Version != bundle.Version {
			return errors.New("policy changed during approval; rerun for a new decision")
		}
		if _, err := ciDecision(fresh, s.Caller, *environment, argv); err != nil {
			return err
		}
	}
	if err := audit("execution.started", nil); err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	// No interactive stdin and no Rein/OIDC bearer passed to the child process.
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "REIN_WORKLOAD_TOKEN=") || strings.HasPrefix(value, "ACTIONS_ID_TOKEN_REQUEST_TOKEN=") {
			continue
		}
		command.Env = append(command.Env, value)
	}
	runErr := ciExecute(command)
	event := "execution.completed"
	if runErr != nil {
		event = "execution.failed"
	}
	if err := audit(event, map[string]any{"success": runErr == nil}); err != nil {
		return fmt.Errorf("command finished but outcome audit failed: %w", err)
	}
	if runErr != nil {
		return fmt.Errorf("CI command failed: %w", runErr)
	}
	return nil
}
