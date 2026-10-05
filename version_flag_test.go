package main

import (
	"bytes"
	"testing"
)

// Agents setting up mrl reach for `--version`; it must answer like `mrl version`.
func TestVersionFlagPrintsVersion(t *testing.T) {
	command := newRootCmd()
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--version"})
	if err := command.Execute(); err != nil {
		t.Fatalf("mrl --version: %v", err)
	}
	if got, want := out.String(), "mrl "+version+"\n"; got != want {
		t.Fatalf("mrl --version printed %q, want %q", got, want)
	}
}
