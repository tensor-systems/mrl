package main

import "testing"

func TestNewAgentCmd_ExecutionCommandsRemoved_LocalLoopRemains(t *testing.T) {
	cmd := newAgentCmd()
	if _, _, err := cmd.Find([]string{"loop"}); err != nil {
		t.Fatalf("agent loop command missing: %v", err)
	}
	for _, name := range []string{"run", "test", "replay"} {
		found, args, err := cmd.Find([]string{name})
		if err == nil && found != cmd && len(args) == 0 {
			t.Fatalf("agent %s command is still registered", name)
		}
	}
}
