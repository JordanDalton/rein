package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/jordandalton/rein/internal/risk"
	"github.com/jordandalton/rein/internal/runner"
	"github.com/jordandalton/rein/internal/spec"
)

// A governed session never falls back to the shared on-disk policy cache.
type mcpGoverned struct {
	request    func(context.Context, string, string, any, any) error
	caller     string
	operation  map[string]any
	policyHash string
}

func newMCPGoverned(caller string) (*mcpGoverned, error) {
	p, err := loadCloudProfile()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("governed MCP requires rein login")
	}
	token, err := loadCloudCredential(p.ControlURL)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("missing control plane credential")
	}
	u, err := url.Parse(p.ControlURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("governed MCP requires an HTTPS control plane origin")
	}
	s := ciSession{Endpoint: p.ControlURL, Token: token, Caller: caller}
	return &mcpGoverned{request: s.request, caller: caller}, nil
}

func (g *mcpGoverned) bundle(ctx context.Context) (ciBundle, error) {
	var b ciBundle
	if err := g.request(ctx, http.MethodGet, "policy-bundles/latest", nil, &b); err != nil {
		return b, err
	}
	// Validate the entire bundle, including expiry, even before planning.
	_, err := ciDecision(b, g.caller, "", []string{"__validate__"})
	return b, err
}

func governedHash(operation map[string]any) string {
	data, _ := json.Marshal(operation)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (g *mcpGoverned) authorize(ctx context.Context, tool, intent string, argv []string, level risk.Level) error {
	b, err := g.bundle(ctx)
	if err != nil {
		return err
	}
	decision, err := ciDecision(b, g.caller, "", argv)
	if err != nil {
		return err
	}
	if decision == "deny" {
		return errors.New("blocked by policy: no explicit allow or approval rule")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	g.operation = map[string]any{"tool": tool, "intent": intent, "argv": append([]string(nil), argv...), "command": runner.Quote(argv), "cwd": cwd, "caller": g.caller, "policy_version": b.Version}
	g.policyHash = governedHash(map[string]any{"bundle": b})
	g.operation["policy_hash"] = g.policyHash
	if decision == "allow" && level == risk.Safe {
		return nil
	}
	hash := governedHash(g.operation)
	var check struct {
		Approved bool `json:"approved"`
	}
	if err := g.request(ctx, http.MethodGet, "approvals/check?operation_hash="+hash+"&caller="+url.QueryEscape(g.caller), nil, &check); err != nil {
		return err
	}
	if check.Approved {
		return nil
	}
	if err := g.request(ctx, http.MethodPost, "approvals", map[string]any{"caller": g.caller, "operation": g.operation, "operation_hash": hash}, nil); err != nil {
		return err
	}
	return errors.New("needs-approval: exact command submitted to Rein Control; approve there and retry the same operation. Do not bypass Rein or increase local approval flags")
}

func (g *mcpGoverned) audit(ctx context.Context, event string, result *runner.Result, runErr error) error {
	if event == "execution_started" {
		b, err := g.bundle(ctx)
		if err != nil {
			return err
		}
		if governedHash(map[string]any{"bundle": b}) != g.policyHash {
			return errors.New("policy changed during authorization; retry for a fresh decision")
		}
	}
	metadata := map[string]any{}
	if result != nil {
		metadata["exit_code"] = result.ExitCode
		metadata["timed_out"] = result.TimedOut
	}
	if runErr != nil {
		metadata["execution_error"] = true
	}
	return g.request(ctx, http.MethodPost, "audit-events", map[string]any{"caller": g.caller, "event": event, "operation": g.operation, "metadata": metadata}, nil)
}

func (d *mcpDeps) toolSpec(ctx context.Context, tool string, refresh bool) (*spec.Spec, error) {
	if d.caller == "" {
		return d.newSpec(ctx, tool, refresh)
	}
	if filepath.Base(tool) != tool || refresh {
		return nil, errors.New("governed MCP cannot discover or refresh executables; ask a trusted operator to run rein spec TOOL first")
	}
	s, err := spec.Load(tool)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("no trusted cached spec for %s; ask a trusted operator to run rein spec %s first", tool, tool)
	}
	return s, nil
}
