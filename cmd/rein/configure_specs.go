package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

var setupToolName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func checkRequiredSpecs(ctx context.Context, names string) error {
	d := &mcpDeps{caller: "setup"}
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if !setupToolName.MatchString(name) {
			return fmt.Errorf("invalid required tool name %q; use comma-separated program names", name)
		}
		if _, err := d.toolSpec(ctx, name, false); err != nil {
			return err
		}
	}
	return nil
}
