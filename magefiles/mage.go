//go:build ignore

// Zero-install Mage entrypoint (https://magefile.org/zeroinstall/), kept in
// its own module (see go.mod in this directory) so mage's own dependency
// never shows up in the main module's go.mod. Run from the repo root:
//
//	go run -C magefiles mage.go -w .. <target>
//
// -C magefiles resolves this file's import against magefiles/go.mod;
// -w .. runs the target's shell commands (go build, go test, ...) back
// against the repo root instead of this directory. The real targets live
// in magefile.go, right next to this file.
package main

import (
	"os"

	"github.com/magefile/mage/mage"
)

func main() { os.Exit(mage.Main()) }
