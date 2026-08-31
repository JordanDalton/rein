package planner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultModel is used when no model is configured. Choosing the next argv
// from help text and emitting a small JSON plan is comfortably within Haiku's
// ability, and it answers several times faster than the Opus-class models;
// pass --model to trade latency for depth on gnarlier tools.
const DefaultModel = "claude-haiku-4-5-20251001"

// API talks to the Claude Messages API directly. Use it when rein ins
// somewhere without the Claude Code CLI installed.
type API struct {
	Model  string
	client anthropic.Client
}

// NewAPI builds an API backend. Credentials are resolved by the SDK: the
// ANTHROPIC_API_KEY env var, or an `ant auth login` profile on disk.
func NewAPI(model string) *API {
	if model == "" {
		model = DefaultModel
	}
	return &API{Model: model, client: anthropic.NewClient(option.WithMaxRetries(2))}
}

func (a *API) Name() string { return "api:" + a.Model }

func (a *API) Complete(ctx context.Context, system, user string) (string, error) {
	resp, err := a.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(a.Model),
		MaxTokens: 8192,
		System: []anthropic.TextBlockParam{{
			Text: system,
			// The system prompt and the capability digest are byte-identical on
			// every step of a run, so caching them turns a multi-step loop into
			// one full-price request plus cheap reads.
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		var apierr *anthropic.Error
		if errors.As(err, &apierr) {
			switch apierr.StatusCode {
			case 401, 403:
				return "", fmt.Errorf("Claude API rejected the credentials — set ANTHROPIC_API_KEY or run `ant auth login` (or use --backend claude-cli): %w", err)
			case 429:
				return "", fmt.Errorf("rate limited by the Claude API: %w", err)
			}
		}
		return "", err
	}

	if resp.StopReason == anthropic.StopReasonRefusal {
		return "", fmt.Errorf("the model declined this request (%s)", resp.StopDetails.Category)
	}

	var b strings.Builder
	for _, block := range resp.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			b.WriteString(t.Text)
		}
	}
	if b.Len() == 0 {
		return "", errors.New("model returned no text content")
	}
	return b.String(), nil
}
