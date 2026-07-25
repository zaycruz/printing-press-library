// Copyright 2026 The plane-pp-cli authors. Licensed under Apache-2.0. See LICENSE.

package cli

import "testing"

func TestAgentNativeWorkflowCommandsRegisteredWithExactReadOnlyAnnotations(t *testing.T) {
	root := RootCmd()
	workflow, _, err := root.Find([]string{"workflow"})
	if err != nil {
		t.Fatalf("find workflow command: %v", err)
	}

	readOnly := map[string]bool{
		"cycle-health":   true,
		"backlog-triage": true,
		"project-health": true,
		"blockers":       true,
	}
	for _, name := range []string{
		"cycle-plan",
		"cycle-health",
		"cycle-rollover",
		"backlog-triage",
		"backlog-apply",
		"project-health",
		"blockers",
	} {
		cmd, _, findErr := workflow.Find([]string{name})
		if findErr != nil || cmd == workflow || cmd.Name() != name {
			t.Fatalf("workflow %s not registered: command=%v err=%v", name, cmd, findErr)
		}
		got := cmd.Annotations != nil && cmd.Annotations["mcp:read-only"] == "true"
		if got != readOnly[name] {
			t.Errorf("workflow %s mcp:read-only=%v, want %v", name, got, readOnly[name])
		}
	}
}

func TestRelationFanoutWorkflowCommandsExposeLimit(t *testing.T) {
	root := RootCmd()
	for _, path := range [][]string{
		{"workflow", "project-health"},
		{"workflow", "blockers"},
	} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %v: %v", path, err)
		}
		if cmd.Flags().Lookup("limit") == nil {
			t.Errorf("%v has no --limit flag", path)
		}
	}
}
