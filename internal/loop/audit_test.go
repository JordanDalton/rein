package loop

import (
	"context"
	"errors"
	"github.com/jordandalton/rein/internal/runner"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExecutionAuditFailures(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "after"}[before], func(t *testing.T) {
			t.Setenv("REIN_HOME", t.TempDir())
			marker := filepath.Join(t.TempDir(), "marker")
			be := &fakeBackend{replies: []string{`{"action":"run","argv":["touch","` + marker + `"],"risk":"safe"}`}}
			cfg := newConfig(t, "touch", be, "")
			cfg.Approval = ApproveAll
			if before {
				cfg.BeforeExecute = func([]string) error { return errors.New("offline") }
			} else {
				cfg.AfterExecute = func(_ []string, r *runner.Result, err error) error {
					if r == nil || err != nil {
						t.Fatal("missing result")
					}
					return errors.New("offline")
				}
			}
			_, err := Run(context.Background(), cfg)
			if err == nil || !strings.Contains(err.Error(), "audit failed") {
				t.Fatal(err)
			}
			_, statErr := os.Stat(marker)
			if before && !os.IsNotExist(statErr) {
				t.Fatal("executed despite pre-audit failure")
			}
			if !before && statErr != nil {
				t.Fatal("expected command to execute")
			}
		})
	}
}
