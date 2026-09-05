package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/jordandalton/rein/internal/runner"
)

const toolGuardReason = "This project routes operations through Rein MCP. Use Rein; do not fall back to native tools, other servers, or delegated agents."

// Capture a small operation summary, never file contents or the entire hook payload.
func toolGuardOperation(tool string, input map[string]any) map[string]any {
	if tool == "" {
		tool = "unknown"
	}
	tool, _ = runner.Redact(tool)
	operation := map[string]any{"tool": tool}
	for _, key := range []string{"command", "file_path", "path", "query"} {
		if value, ok := input[key].(string); ok {
			value, _ = runner.Redact(value)
			if len(value) > 4096 {
				value = value[:4096] + "…"
			}
			operation[key] = value
		}
	}
	return operation
}

func recordToolGuardBlock(ctx context.Context, operation map[string]any) error {
	profile, err := loadCloudProfile()
	if err != nil {
		return err
	}
	if profile == nil {
		return errors.New("Rein Control is not connected")
	}
	token, err := loadCloudCredentialContext(ctx, profile.ControlURL)
	if err != nil {
		return err
	}
	return cloudJSON(ctx, http.MethodPost, profile.ControlURL+"/api/v1/rein/audit-events", token, map[string]any{
		"event": "blocked", "caller": "tool-guard", "operation": operation,
		"metadata": map[string]any{"executed": false, "stage": "tool_guard", "reason": toolGuardReason},
	}, nil)
}
