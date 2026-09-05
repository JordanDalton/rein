package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRepairPersistentMissing(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "protect edits"}[drift], func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(root)
			if err := configurePersistent("claude-code", "", "", false, true, false, false); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(".claude/settings.json")
			receipt, _ := os.ReadFile(".rein/harnesses/claude-code.persistent.json")
			if err := os.Remove(".claude/settings.json"); err != nil {
				t.Fatal(err)
			}
			if drift {
				if err := os.WriteFile(".mcp.json", []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := repairPersistentMissing("claude-code", true); err == nil {
					t.Fatal("accepted edited file")
				}
				if _, err := os.Stat(".claude/settings.json"); !os.IsNotExist(err) {
					t.Fatal("wrote before validating all files")
				}
				return
			}
			missing, err := repairPersistentMissing("claude-code", false)
			if err != nil || len(missing) != 1 {
				t.Fatalf("preview: %v %v", missing, err)
			}
			if _, err := os.Stat(".claude/settings.json"); !os.IsNotExist(err) {
				t.Fatal("preview wrote file")
			}
			if _, err := repairPersistentMissing("claude-code", true); err != nil {
				t.Fatal(err)
			}
			restored, _ := os.ReadFile(".claude/settings.json")
			if !bytes.Equal(original, restored) {
				t.Fatal("restored bytes differ")
			}
			after, _ := os.ReadFile(".rein/harnesses/claude-code.persistent.json")
			if !bytes.Equal(receipt, after) {
				t.Fatal("receipt changed")
			}
			if err := configurePersistent("claude-code", "", "", false, false, false, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
