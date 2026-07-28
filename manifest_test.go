package main

import (
	"testing"

	"github.com/BurntSushi/toml"
)

// TestManifestVersionMatchesBinary keeps herdr-plugin.toml's version and the
// binary's version const from drifting: herdr reports the manifest version, so
// a mismatch misstates which build is installed.
func TestManifestVersionMatchesBinary(t *testing.T) {
	var m struct {
		Version string `toml:"version"`
	}
	if _, err := toml.DecodeFile("herdr-plugin.toml", &m); err != nil {
		t.Fatalf("herdr-plugin.toml is not valid TOML: %v", err)
	}
	if m.Version != version {
		t.Errorf("herdr-plugin.toml version = %q, want %q (main.go const)", m.Version, version)
	}
}
