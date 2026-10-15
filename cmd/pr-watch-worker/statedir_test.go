package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirHonorsXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	got, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir() error = %v", err)
	}
	if want := "/custom/state/pr-watch-mcp"; got != want {
		t.Errorf("stateDir() = %q, want %q", got, want)
	}
}

func TestStateDirFallsBackToLocalState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/someone")
	got, err := stateDir()
	if err != nil {
		t.Fatalf("stateDir() error = %v", err)
	}
	if want := "/home/someone/.local/state/pr-watch-mcp"; got != want {
		t.Errorf("stateDir() = %q, want %q", got, want)
	}
}

// TestMigrateLegacyRegistryMovesExistingDB: the registry used to live under
// the config dir (wrong — it's state). An existing database holds real
// watch rows and permanent merged/closed badges, so it must be moved, not
// abandoned.
func TestMigrateLegacyRegistryMovesExistingDB(t *testing.T) {
	legacy, current := t.TempDir(), filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(filepath.Join(legacy, "registry.db"), []byte("real data"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyRegistry(legacy, current)
	if err != nil {
		t.Fatalf("migrateLegacyRegistry() error = %v", err)
	}
	if !migrated {
		t.Error("migrateLegacyRegistry() = false, want true")
	}
	got, err := os.ReadFile(filepath.Join(current, "registry.db"))
	if err != nil || string(got) != "real data" {
		t.Errorf("new registry.db = %q, %v, want the original contents", got, err)
	}
	if _, err := os.Stat(filepath.Join(legacy, "registry.db")); !os.IsNotExist(err) {
		t.Errorf("legacy registry.db still present (err = %v), want it moved away", err)
	}
}

// TestMigrateLegacyRegistryNeverOverwritesNewDB: if a database already
// exists at the new location it is the live one; the legacy file must be
// left untouched, never clobbering it.
func TestMigrateLegacyRegistryNeverOverwritesNewDB(t *testing.T) {
	legacy, current := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(legacy, "registry.db"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "registry.db"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}

	migrated, err := migrateLegacyRegistry(legacy, current)
	if err != nil {
		t.Fatalf("migrateLegacyRegistry() error = %v", err)
	}
	if migrated {
		t.Error("migrateLegacyRegistry() = true, want false when the new DB already exists")
	}
	got, _ := os.ReadFile(filepath.Join(current, "registry.db"))
	if string(got) != "live" {
		t.Errorf("new registry.db = %q, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(legacy, "registry.db")); err != nil {
		t.Errorf("legacy registry.db error = %v, want it left in place", err)
	}
}

func TestMigrateLegacyRegistryNoopWithoutLegacyDB(t *testing.T) {
	migrated, err := migrateLegacyRegistry(t.TempDir(), t.TempDir())
	if err != nil || migrated {
		t.Errorf("migrateLegacyRegistry() = %v, %v, want false, nil on a fresh install", migrated, err)
	}
}
