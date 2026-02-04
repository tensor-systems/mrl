package main

import (
	"strings"
	"testing"

	"github.com/modelrelay/modelrelay/platform/rlm"
)

func TestBuildRLMSystemAdditions_IncludesLimits(t *testing.T) {
	prompt := rlm.BuildRunnerSystemAdditions("", 2, 10)
	if !strings.Contains(prompt, "max_depth=2") {
		t.Fatalf("prompt missing max_depth: %s", prompt)
	}
	if !strings.Contains(prompt, "max_subcalls=10") {
		t.Fatalf("prompt missing max_subcalls: %s", prompt)
	}
	if !strings.Contains(prompt, "answer['content']") {
		t.Fatalf("prompt missing answer guidance: %s", prompt)
	}
}

func TestBuildRLMSystemAdditions_PrependsCustom(t *testing.T) {
	prompt := rlm.BuildRunnerSystemAdditions("Custom", 1, 1)
	if !strings.HasPrefix(prompt, "Custom") {
		t.Fatalf("prompt missing custom prefix: %s", prompt)
	}
}
