package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cwrau/aoe-plugin-pr-watch-mcp/internal/registry"
)

// checkoutWithEmail makes a git checkout whose own config sets user.email, the
// way a directory-scoped identity would for one project.
func checkoutWithEmail(t *testing.T, email string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", dir},
		{"-C", dir, "config", "user.email", email},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Skipf("git %v failed (%v): %s", args, err, out)
		}
	}
	return dir
}

func TestEmailFromCheckoutsTakesTheFirstProjectIdentity(t *testing.T) {
	a := checkoutWithEmail(t, "a@project-a.example")
	b := checkoutWithEmail(t, "b@project-b.example")
	if got := emailFromCheckouts(context.Background(), []string{a, b}); got != "a@project-a.example" {
		t.Errorf("emailFromCheckouts() = %q, want the first checkout's own email", got)
	}
	if got := emailFromCheckouts(context.Background(), []string{filepath.Join(t.TempDir(), "gone"), b}); got != "b@project-b.example" {
		t.Errorf("emailFromCheckouts() = %q, want the next checkout when one is not a git directory", got)
	}
	if got := emailFromCheckouts(context.Background(), nil); got != "" {
		t.Errorf("emailFromCheckouts(nil) = %q, want none", got)
	}
}

// The project's checkout, not the worker's own working directory, decides which
// identity applies: two repositories may use two different emails.
func TestGitEmailForReadsTheCheckoutOfThatRepository(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	acme := checkoutWithEmail(t, "me@acme.example")
	other := checkoutWithEmail(t, "me@other.example")
	for path, key := range map[string]string{acme: "acme/widgets", other: "other/gadgets"} {
		if err := store.UpsertAutoDiscoveryTarget(ctx, "s", path, "bitbucket", key, "b"); err != nil {
			t.Fatalf("UpsertAutoDiscoveryTarget() error = %v", err)
		}
	}

	for _, tt := range []struct{ workspace, repo, want string }{
		{"acme", "widgets", "me@acme.example"},
		{"other", "gadgets", "me@other.example"},
	} {
		if got := gitEmailFor(ctx, store, tt.workspace, tt.repo); got != tt.want {
			t.Errorf("gitEmailFor(%s/%s) = %q, want %q", tt.workspace, tt.repo, got, tt.want)
		}
	}
}

// With no known checkout (a PR registered by hand for a repository no session
// has open), the identity the worker itself runs under is the best left.
func TestGitEmailForFallsBackToTheWorkersOwnIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := registry.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatalf("registry.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	want, _ := exec.Command("git", "config", "--get", "user.email").Output()
	got := gitEmailFor(ctx, store, "nobody", "nowhere")
	if got != trimNL(string(want)) {
		t.Errorf("gitEmailFor() = %q, want the worker's own git email %q", got, trimNL(string(want)))
	}
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
