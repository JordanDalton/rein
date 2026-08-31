package planner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testBackend(t *testing.T, h http.HandlerFunc) (*OpenAI, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	return &OpenAI{BaseURL: srv.URL, Model: "test-model", Label: "test", HTTP: srv.Client()}, srv.Close
}

func reply(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatResponse{
		Choices: []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		}{{Message: chatMessage{Role: "assistant", Content: content}}},
	})
}

func TestOpenAISendsSystemAndUserAndReturnsContent(t *testing.T) {
	var got chatRequest
	be, done := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		reply(w, `{"action":"answer","answer":"ok"}`)
	})
	defer done()

	out, err := be.Complete(context.Background(), "SYS", "USER")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"action":"answer","answer":"ok"}` {
		t.Errorf("content = %q", out)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "SYS" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if got.Messages[1].Content != "USER" {
		t.Errorf("user message = %q", got.Messages[1].Content)
	}
	if got.ResponseFormat == nil || got.ResponseFormat.Type != "json_object" {
		t.Error("JSON mode was not requested")
	}
}

// Endpoints that reject response_format or a non-default temperature should be
// retried without them rather than failing the run.
func TestOpenAIRetriesWithoutOptionalFields(t *testing.T) {
	var calls int
	var second chatRequest
	be, done := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Unsupported value: 'temperature' does not support 0"}}`)
			return
		}
		json.Unmarshal(body, &second)
		reply(w, "recovered")
	})
	defer done()

	out, err := be.Complete(context.Background(), "s", "u")
	if err != nil {
		t.Fatal(err)
	}
	if out != "recovered" || calls != 2 {
		t.Errorf("out=%q calls=%d", out, calls)
	}
	if second.Temperature != nil || second.ResponseFormat != nil {
		t.Error("retry still carried the rejected fields")
	}
}

func TestOpenAISurfacesAPIErrors(t *testing.T) {
	be, done := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	})
	defer done()

	_, err := be.Complete(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Errorf("error = %v", err)
	}
}

func TestOpenAIRejectsEmptyChoices(t *testing.T) {
	be, done := testBackend(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	})
	defer done()

	if _, err := be.Complete(context.Background(), "s", "u"); err == nil {
		t.Error("expected an error for an empty choices array")
	}
}

func TestNewOpenAIRequiresAModel(t *testing.T) {
	_, err := NewOpenAI("ollama", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "ollama list") {
		t.Errorf("error should name the model flag and how to find one, got: %v", err)
	}
}

func TestNewOpenAIRequiresACredentialWhereOneIsNeeded(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("REIN_API_KEY", "")
	if _, err := NewOpenAI("openai", "", "some-model", ""); err == nil {
		t.Error("expected an error when OPENAI_API_KEY is unset")
	}
	// Local servers need no credential.
	if _, err := NewOpenAI("ollama", "", "qwen2.5", ""); err != nil {
		t.Errorf("ollama should not require a key: %v", err)
	}
}

// Credential resolution order: --api-key-env, then REIN_API_KEY, then the
// provider's conventional variable.
func TestCredentialResolutionOrder(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "conventional")
	t.Setenv("REIN_API_KEY", "rein-wide")
	t.Setenv("MY_OWN_KEY", "explicit")

	be, err := NewOpenAI("openai", "", "m", "MY_OWN_KEY")
	if err != nil || be.APIKey != "explicit" {
		t.Errorf("--api-key-env should win: key=%q err=%v", be.APIKey, err)
	}

	be, err = NewOpenAI("openai", "", "m", "")
	if err != nil || be.APIKey != "rein-wide" {
		t.Errorf("REIN_API_KEY should outrank the provider default: key=%q err=%v", be.APIKey, err)
	}

	t.Setenv("REIN_API_KEY", "")
	be, err = NewOpenAI("openai", "", "m", "")
	if err != nil || be.APIKey != "conventional" {
		t.Errorf("provider default should be the fallback: key=%q err=%v", be.APIKey, err)
	}
}

// A provider with no preset is reachable via openai-compatible + --base-url.
func TestOpenAICompatibleEscapeHatch(t *testing.T) {
	t.Setenv("REIN_API_KEY", "k")
	be, err := NewOpenAI("openai-compatible", "https://example.test/v1", "some-model", "")
	if err != nil {
		t.Fatal(err)
	}
	if be.BaseURL != "https://example.test/v1" {
		t.Errorf("BaseURL = %q", be.BaseURL)
	}
	// ...but it is useless without one.
	if _, err := NewOpenAI("openai-compatible", "", "some-model", ""); err == nil {
		t.Error("expected an error when no base URL is available")
	}
}

func TestNewOpenAIBaseURLOverride(t *testing.T) {
	be, err := NewOpenAI("ollama", "http://elsewhere:9999/v1/", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	if be.BaseURL != "http://elsewhere:9999/v1" {
		t.Errorf("BaseURL = %q (trailing slash should be trimmed)", be.BaseURL)
	}
}

func TestNewOpenAIRejectsUnknownPreset(t *testing.T) {
	_, err := NewOpenAI("no-such-provider", "", "m", "")
	if err == nil {
		t.Fatal("expected an error for an unknown backend name")
	}
	// The error must list what is actually available.
	for _, want := range []string{"openrouter", "ollama", "codex-cli"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list %q, got: %v", want, err)
		}
	}
}

func TestBackendNamesAreStable(t *testing.T) {
	names := BackendNames()
	if len(names) < 12 {
		t.Errorf("expected every backend to be listed, got %v", names)
	}
	if names[0] != "claude-cli" {
		t.Errorf("the default backend should be listed first, got %v", names)
	}
}

func TestPlannerTimeoutIsOverridable(t *testing.T) {
	t.Setenv("REIN_PLANNER_TIMEOUT", "15m")
	if got := plannerTimeout(); got.Minutes() != 15 {
		t.Errorf("plannerTimeout() = %v, want 15m", got)
	}
	// A malformed value must fall back rather than produce a zero timeout,
	// which http.Client reads as "no timeout at all".
	t.Setenv("REIN_PLANNER_TIMEOUT", "soon")
	if got := plannerTimeout(); got != defaultPlannerTimeout {
		t.Errorf("plannerTimeout() = %v, want the default", got)
	}
}
