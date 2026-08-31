package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const modulePath = "github.com/jordandalton/rein"

// version reads the module version stamped into the binary by `go install
// module@version`. Source builds report "(devel)".
func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "unknown"
}

func cmdVersion() error {
	fmt.Printf("rein %s\n", version())
	return nil
}

// cmdUpdate reinstalls rein at @latest with the Go toolchain — the same
// command the README's install step uses, so it updates exactly what that
// flow installed.
func cmdUpdate(ctx context.Context) error {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("updating uses the Go toolchain and `go` is not on PATH — install Go, or rebuild from a clone of " + modulePath)
	}

	// Source builds are stamped "(devel)" or with a "+dirty" VCS suffix.
	cur := version()
	if cur == "(devel)" || strings.Contains(cur, "+dirty") {
		return errors.New("this rein is a source build, not `go install`ed — update it with `git pull && go build` in your clone instead")
	}

	fmt.Printf("current: rein %s\nfetching latest…\n", cur)
	install := exec.CommandContext(ctx, goBin, "install", modulePath+"/cmd/rein@latest")
	install.Stdout, install.Stderr = os.Stdout, os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("go install failed: %w", err)
	}

	dir := installDir(ctx, goBin)
	installed := filepath.Join(dir, "rein")
	if out, err := exec.CommandContext(ctx, installed, "version").Output(); err == nil {
		fmt.Printf("updated: %s", out)
	} else {
		fmt.Printf("updated (installed to %s)\n", dir)
	}

	// `go install` writes to GOBIN, which is not necessarily the binary that
	// is running right now (e.g. a copy elsewhere on PATH). Say so, or the
	// user's next `rein` may silently still be the old one.
	if self, err := os.Executable(); err == nil {
		if rs, err2 := filepath.EvalSymlinks(self); err2 == nil {
			self = rs
		}
		if target, err2 := filepath.EvalSymlinks(installed); err2 == nil && self != target {
			fmt.Printf("note: you are running %s, but the update landed in %s\n", self, installed)
		}
	}
	return nil
}

// installDir resolves where `go install` puts binaries: GOBIN, else GOPATH/bin.
func installDir(ctx context.Context, goBin string) string {
	if out, err := exec.CommandContext(ctx, goBin, "env", "GOBIN").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	if out, err := exec.CommandContext(ctx, goBin, "env", "GOPATH").Output(); err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return filepath.Join(d, "bin")
		}
	}
	return ""
}
