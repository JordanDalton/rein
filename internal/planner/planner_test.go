package planner

import (
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
	sp := SystemPrompt()
	for _, want := range []string{"consequence", "cannot be undone", "may not be able to read shell"} {
		if !strings.Contains(sp, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
}
