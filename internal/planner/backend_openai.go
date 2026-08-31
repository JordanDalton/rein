package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// OpenAI talks to any endpoint speaking the OpenAI chat-completions wire
// format. That one implementation covers OpenAI itself, Groq, Together,
// OpenRouter, most corporate gateways, and Ollama's compatibility endpoint —
// they differ only in base URL, credential, and model name.
//
// The planner needs nothing but text in and text out, so there is deliberately
// no dependency on provider-specific tool-calling or structured-output APIs.
type OpenAI struct {
	BaseURL string // e.g. https://api.openai.com/v1
	Model   string
	APIKey  string // may be empty for local servers
	Label   string // what to call this backend in output
	HTTP    *http.Client
}

// preset is a known OpenAI-compatible endpoint. They differ only in base URL
// and which environment variable holds the credential, which is why one
// implementation covers all of them.
type preset struct {
	baseURL string
	keyEnv  string // empty means the endpoint needs no credential
}

var presets = map[string]preset{
	"openai":     {"https://api.openai.com/v1", "OPENAI_API_KEY"},
	"openrouter": {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
	"xai":        {"https://api.x.ai/v1", "XAI_API_KEY"},
	"groq":       {"https://api.groq.com/openai/v1", "GROQ_API_KEY"},
	"deepseek":   {"https://api.deepseek.com/v1", "DEEPSEEK_API_KEY"},
	"mistral":    {"https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
	"together":   {"https://api.together.xyz/v1", "TOGETHER_API_KEY"},
	"ollama":     {"http://localhost:11434/v1", ""},
	"lmstudio":   {"http://localhost:1234/v1", ""},
	// An escape hatch for anything not listed: supply --base-url yourself.
	"openai-compatible": {"", "REIN_API_KEY"},
}

// BackendNames lists every selectable backend, for help text and errors.
func BackendNames() []string {
	hosted := make([]string, 0, len(presets))
	for n := range presets {
		hosted = append(hosted, n)
	}
	sort.Strings(hosted)
	return append([]string{"claude-cli", "api", "codex-cli", "grok-cli"}, hosted...)
}

// defaultPlannerTimeout is generous because local inference is CPU-bound:
// processing a 6k-token capability digest on a laptop can take minutes before
// the first byte comes back. Override with REIN_PLANNER_TIMEOUT.
const defaultPlannerTimeout = 5 * time.Minute

func plannerTimeout() time.Duration {
	if v := os.Getenv("REIN_PLANNER_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultPlannerTimeout
}

// NewOpenAI builds a backend from a preset name, with optional overrides.
//
// An empty model is an error rather than a default: the right model name
// depends entirely on the endpoint, and guessing produces a confusing 404 from
// the provider instead of a clear message from here.
func NewOpenAI(name, baseURL, model, keyEnv string) (*OpenAI, error) {
	p, ok := presets[name]
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (want one of: %s)", name, strings.Join(BackendNames(), ", "))
	}
	if baseURL == "" {
		baseURL = os.Getenv("REIN_BASE_URL")
	}
	if baseURL == "" {
		baseURL = p.baseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("the %s backend needs --base-url (or REIN_BASE_URL)", name)
	}
	if model == "" {
		hint := "pass --model"
		if p.keyEnv == "" {
			hint = "pass --model (`ollama list` shows what you have)"
		}
		return nil, fmt.Errorf("the %s backend has no default model — %s", name, hint)
	}

	// Credential resolution, most specific first: a variable named on the
	// command line, then a rein-wide override, then the provider's
	// conventional one.
	var key string
	for _, env := range []string{keyEnv, "REIN_API_KEY", p.keyEnv} {
		if env == "" {
			continue
		}
		if v := os.Getenv(env); v != "" {
			key = v
			break
		}
	}
	if key == "" && p.keyEnv != "" {
		want := p.keyEnv
		if keyEnv != "" {
			want = keyEnv
		}
		return nil, fmt.Errorf("the %s backend needs %s to be set (or --api-key-env to name a different variable)", name, want)
	}

	return &OpenAI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Model:   model,
		APIKey:  key,
		Label:   name,
		HTTP:    &http.Client{Timeout: plannerTimeout()},
	}, nil
}

func (o *OpenAI) Name() string { return o.Label + ":" + o.Model }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    *float64      `json:"temperature,omitempty"`
	ResponseFormat *respFormat   `json:"response_format,omitempty"`
}

type respFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (o *OpenAI) Complete(ctx context.Context, system, user string) (string, error) {
	zero := 0.0
	req := chatRequest{
		Model: o.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		// Planning is not a creative task, and JSON mode materially improves
		// the plan-parsing hit rate on smaller local models.
		Temperature:    &zero,
		ResponseFormat: &respFormat{Type: "json_object"},
	}

	body, status, err := o.post(ctx, req)
	// Not every compatible endpoint accepts these two optional fields, and
	// reasoning models often reject a non-default temperature. Retry once
	// without them rather than making the user work out which knob broke.
	if status == http.StatusBadRequest && mentionsOptionalField(body) {
		req.Temperature = nil
		req.ResponseFormat = nil
		body, status, err = o.post(ctx, req)
	}
	if err != nil {
		return "", err
	}

	var resp chatResponse
	if jsonErr := json.Unmarshal(body, &resp); jsonErr != nil {
		return "", fmt.Errorf("%s returned unreadable JSON (HTTP %d): %s", o.Label, status, clip(string(body), 300))
	}
	if resp.Error != nil {
		return "", fmt.Errorf("%s error: %s", o.Label, resp.Error.Message)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d: %s", o.Label, status, clip(string(body), 300))
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("%s returned no choices", o.Label)
	}
	return resp.Choices[0].Message.Content, nil
}

func (o *OpenAI) post(ctx context.Context, r chatRequest) ([]byte, int, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	res, err := o.HTTP.Do(httpReq)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "connection refused"):
			return nil, 0, fmt.Errorf("nothing listening at %s — is the %s server running?", o.BaseURL, o.Label)
		case strings.Contains(err.Error(), "Client.Timeout"):
			return nil, 0, fmt.Errorf("%s did not respond within %s — a local model may just be slow on a prompt this size; raise REIN_PLANNER_TIMEOUT (e.g. 15m) or use a smaller model: %w",
				o.Label, plannerTimeout(), err)
		}
		return nil, 0, fmt.Errorf("%s request failed: %w", o.Label, err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, res.StatusCode, fmt.Errorf("reading %s response: %w", o.Label, err)
	}
	return body, res.StatusCode, nil
}

func mentionsOptionalField(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "response_format") || strings.Contains(s, "temperature")
}
