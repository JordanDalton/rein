package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParsePlanTolerantOfWrapping(t *testing.T) {
	cases := []string{
		`{"action":"run","argv":["git","status"],"risk":"safe"}`,
		"```json\n{\"action\":\"run\",\"argv\":[\"git\",\"status\"],\"risk\":\"safe\"}\n```",
		"Sure — here's the plan:\n{\"action\":\"run\",\"argv\":[\"git\",\"status\"],\"risk\":\"safe\"}\nLet me know.",
	}
	for _, raw := range cases {
		p, err := ParsePlan(raw)
		if err != nil {
			t.Fatalf("ParsePlan(%q) failed: %v", raw, err)
		}
		if p.Action != ActionRun || len(p.Argv) != 2 || p.Argv[1] != "status" {
			t.Errorf("ParsePlan(%q) = %+v", raw, p)
		}
	}
}

func TestExtractJSONIgnoresBracesInStrings(t *testing.T) {
	raw := `{"action":"answer","answer":"the value is {not a brace}"}`
	p, err := ParsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Answer != "the value is {not a brace}" {
		t.Errorf("got %q", p.Answer)
	}
}

func TestParsePlanRejectsBadPlans(t *testing.T) {
	for _, raw := range []string{
		`no json here`,
		`{"action":"run"}`,           // run with no argv
		`{"action":"teleport"}`,      // unknown action
		`{"action":"run","argv":[]}`, // empty argv
	} {
		if _, err := ParsePlan(raw); err == nil {
			t.Errorf("ParsePlan(%q) should have failed", raw)
		}
	}
}

// When a model produces valid JSON that is not a plan, the error must quote it.
// On weaker models this is the only signal the user has to work from.
func TestSchemaErrorsQuoteThePayload(t *testing.T) {
	raw := `{"commits": ["abc123"], "summary": "recent work"}`
	_, err := ParsePlan(raw)
	if err == nil {
		t.Fatal("expected a schema error")
	}
	if !strings.Contains(err.Error(), "commits") {
		t.Errorf("error should quote the offending payload, got: %v", err)
	}
	if !strings.Contains(err.Error(), "run/answer/ask") {
		t.Errorf("error should name the expected actions, got: %v", err)
	}
}

func TestParsePlanCarriesConsequence(t *testing.T) {
	p, err := ParsePlan(`{"action":"run","argv":["git","reset","--hard"],"risk":"danger",
	  "consequence":"This permanently deletes your last commit. It cannot be undone."}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Consequence, "cannot be undone") {
		t.Errorf("Consequence = %q", p.Consequence)
	}
}

// The system prompt has to actually ask for the field, or nothing populates it.
func TestSystemPromptRequestsConsequences(t *testing.T) {
	for _, reasoning := range []bool{false, true} {
		sp := SystemPrompt(reasoning)
		for _, want := range []string{"consequence", "cannot be undone", "may not be able to read shell"} {
			if !strings.Contains(sp, want) {
				t.Errorf("system prompt (reasoning=%v) is missing %q", reasoning, want)
			}
		}
		if got := strings.Contains(sp, `"reasoning"`); got != reasoning {
			t.Errorf("reasoning=%v but the schema mentions the field: %v", reasoning, got)
		}
	}
	if strings.Contains(SystemPrompt(false), "@@") {
		t.Error("schema placeholder was not substituted")
	}
}

// sessionBackend fakes a Sessional backend and records what it was sent.
type sessionBackend struct {
	msgs    []string
	replies []string
	fail    map[int]bool // Send calls (0-based) that error
	calls   int
}

func (s *sessionBackend) Name() string { return "fake-session" }
func (s *sessionBackend) Complete(_ context.Context, _, u string) (string, error) {
	return s.Send(nil, "", u)
}
func (s *sessionBackend) Send(_ context.Context, _, msg string) (string, error) {
	n := s.calls
	s.calls++
	if s.fail[n] {
		return "", errors.New("session died")
	}
	s.msgs = append(s.msgs, msg)
	return s.replies[len(s.msgs)-1], nil
}

func TestSessionSendsOnlyNewSteps(t *testing.T) {
	be := &sessionBackend{replies: []string{
		`{"action":"run","argv":["git","status"],"risk":"safe"}`,
		`{"action":"answer","answer":"done"}`,
	}}
	sess := NewSession(be, "git", "DIGEST", "what changed?")

	if _, err := sess.Next(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	steps := []Step{{Argv: []string{"git", "status"}, Stdout: "clean tree"}}
	if _, err := sess.Next(context.Background(), steps); err != nil {
		t.Fatal(err)
	}

	if len(be.msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(be.msgs))
	}
	if !strings.Contains(be.msgs[0], "what changed?") {
		t.Errorf("opening message should carry the intent: %q", be.msgs[0])
	}
	if strings.Contains(be.msgs[1], "what changed?") {
		t.Errorf("delta message should not resend the intent: %q", be.msgs[1])
	}
	if !strings.Contains(be.msgs[1], "clean tree") || !strings.Contains(be.msgs[1], "[step 1]") {
		t.Errorf("delta message should carry the new step with stable numbering: %q", be.msgs[1])
	}
}

func TestSessionRebuildsAfterSendFailure(t *testing.T) {
	be := &sessionBackend{
		replies: []string{
			`{"action":"run","argv":["git","status"],"risk":"safe"}`,
			`{"action":"answer","answer":"done"}`,
		},
		fail: map[int]bool{1: true}, // second Send dies mid-run
	}
	sess := NewSession(be, "git", "DIGEST", "what changed?")

	if _, err := sess.Next(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	steps := []Step{{Argv: []string{"git", "status"}, Stdout: "clean tree"}}
	if _, err := sess.Next(context.Background(), steps); err != nil {
		t.Fatal(err)
	}
	// The retry message is a full rebuild: intent plus history.
	last := be.msgs[len(be.msgs)-1]
	if !strings.Contains(last, "what changed?") || !strings.Contains(last, "clean tree") {
		t.Errorf("retry after a dead session should resend the full transcript: %q", last)
	}
}

func TestBuildUserTrimsOldOutput(t *testing.T) {
	long := strings.Repeat("x", 5000)
	var steps []Step
	for i := 0; i < keepFullSteps+1; i++ {
		steps = append(steps, Step{Argv: []string{"git", "log"}, Stdout: long})
	}
	msg := BuildUser("git", "intent", steps)
	if !strings.Contains(msg, "bytes trimmed") {
		t.Error("oldest step's output should be trimmed")
	}
	// Only the one step past the keep-full window gets trimmed.
	if got := strings.Count(msg, "bytes trimmed"); got != 1 {
		t.Errorf("expected exactly 1 trimmed step, got %d", got)
	}
}

func TestClipMiddleKeepsHeadAndTail(t *testing.T) {
	s := "HEAD" + strings.Repeat("m", 2000) + "TAIL"
	out := clipMiddle(s, 100)
	if !strings.HasPrefix(out, "HEAD") || !strings.HasSuffix(out, "TAIL") {
		t.Errorf("clipMiddle should keep both ends: %q", out)
	}
	if len(out) > 200 {
		t.Errorf("clipMiddle output too long: %d bytes", len(out))
	}
	if clipMiddle("short", 100) != "short" {
		t.Error("clipMiddle should pass short strings through")
	}
}

func TestSystemCarriesDigest(t *testing.T) {
	sys := BuildSystem("THE-DIGEST", false)
	if !strings.Contains(sys, "THE-DIGEST") || !strings.Contains(sys, "TOOL CAPABILITIES") {
		t.Errorf("BuildSystem should embed the digest: %q", sys)
	}
}

// Without extended thinking, weaker models occasionally omit the action field
// while filling in exactly one payload field; ParsePlan repairs those.
func TestParsePlanInfersMissingAction(t *testing.T) {
	p, err := ParsePlan(`{"answer":"you are on main"}`)
	if err != nil || p.Action != ActionAnswer {
		t.Errorf("bare answer should infer action=answer, got %+v, %v", p, err)
	}
	p, err = ParsePlan(`{"argv":["git","status"],"risk":"safe"}`)
	if err != nil || p.Action != ActionRun {
		t.Errorf("bare argv should infer action=run, got %+v, %v", p, err)
	}
	p, err = ParsePlan(`{"question":"which remote?"}`)
	if err != nil || p.Action != ActionAsk {
		t.Errorf("bare question should infer action=ask, got %+v, %v", p, err)
	}
	// Ambiguous payloads still fail rather than guess.
	if _, err := ParsePlan(`{"answer":"done","argv":["git","push"]}`); err == nil {
		t.Error("ambiguous payload should not be repaired")
	}
}
