//go:build mage

package main

import (
	"fmt"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// Default is what `go run mage.go` (no target given) runs.
var Default = Check

// workerBinary matches aoe-plugin.toml's own [[runtime.build]] command
// exactly, so `mage build` and the plugin's real build step never drift.
const workerBinary = ".aoe-build/pr-watch-worker"

// golangciLintPkg is the `tool` dependency pinned in magefiles/go.mod (via
// `go get -tool`) — go.sum-verified and reproducible. The repo-root
// go.work links this module in for dependency resolution, so `go run` on
// this import path works directly from the repo root: no ambient
// PATH/nix-shell binary, no separate build step to manage.
const golangciLintPkg = "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"

// Build compiles the plugin worker binary.
func Build() error {
	return sh.RunV("go", "build", "-o", workerBinary, "./cmd/pr-watch-worker")
}

// Test runs the full test suite.
func Test() error {
	return sh.RunV("go", "test", "./...")
}

// TestRace runs the full test suite with the race detector.
func TestRace() error {
	return sh.RunV("go", "test", "-race", "./...")
}

// Vet runs go vet.
func Vet() error {
	return sh.RunV("go", "vet", "./...")
}

// Fmt fails and lists any file that isn't gofmt-clean. It never rewrites —
// run `gofmt -w .` yourself if that's what you want.
func Fmt() error {
	out, err := sh.Output("gofmt", "-l", ".")
	if err != nil {
		return err
	}
	if out != "" {
		return fmt.Errorf("gofmt needs to be run on:\n%s", out)
	}
	return nil
}

// Lint runs golangci-lint against the main module.
func Lint() error {
	return sh.RunV("go", "run", golangciLintPkg, "run", "./...")
}

// Check runs everything a change should be gated on, in the order a
// failure is cheapest to see: fmt, vet, lint, then the test suite.
func Check() {
	mg.SerialDeps(Fmt, Vet, Lint, Test)
}
