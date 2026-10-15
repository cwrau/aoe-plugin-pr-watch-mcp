package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const registryFileName = "registry.db"

// stateDir is where the worker keeps its registry: $XDG_STATE_HOME, falling
// back to ~/.local/state. The registry is state (watch rows, last-seen PR
// data, permanent merged/closed badges), not configuration, so it doesn't
// belong under os.UserConfigDir. Go's stdlib has no UserStateDir.
func stateDir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "pr-watch-mcp"), nil
}

// migrateLegacyRegistry moves the registry from the old config-dir location
// to its state-dir home. It never touches an existing database at the new
// location, and does nothing when there's no legacy file. Reports whether a
// move happened.
func migrateLegacyRegistry(legacyDir, newDir string) (bool, error) {
	oldPath := filepath.Join(legacyDir, registryFileName)
	newPath := filepath.Join(newDir, registryFileName)

	if _, err := os.Stat(newPath); err == nil {
		return false, nil
	}
	if _, err := os.Stat(oldPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return false, err
	}

	err := os.Rename(oldPath, newPath)
	if err == nil {
		return true, nil
	}
	// Rename fails across filesystems/subvolumes: copy, and only drop the
	// original once the copy is fully written and synced.
	if !errors.Is(err, syscall.EXDEV) {
		return false, fmt.Errorf("move registry: %w", err)
	}
	if err := copyFileSynced(oldPath, newPath); err != nil {
		_ = os.Remove(newPath) // partial copy; the original is still intact
		return false, fmt.Errorf("copy registry: %w", err)
	}
	if err := os.Remove(oldPath); err != nil {
		return true, fmt.Errorf("remove migrated registry: %w", err)
	}
	return true, nil
}

func copyFileSynced(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
