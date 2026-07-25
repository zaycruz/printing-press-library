// Copyright 2026 The plane-pp-cli authors. Licensed under Apache-2.0. See LICENSE.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

type projectWorkflowCall struct {
	path   string
	params map[string]string
}

type fakeProjectWorkflowReader struct {
	responses map[string][]json.RawMessage
	calls     []projectWorkflowCall
}

func (f *fakeProjectWorkflowReader) GetNoCache(_ context.Context, path string, params map[string]string) (json.RawMessage, error) {
	copied := map[string]string{}
	for key, value := range params {
		copied[key] = value
	}
	f.calls = append(f.calls, projectWorkflowCall{path: path, params: copied})
	queue := f.responses[path]
	if len(queue) == 0 {
		return nil, fmt.Errorf("unexpected GET %s", path)
	}
	f.responses[path] = queue[1:]
	return queue[0], nil
}

func rawJSON(value string) json.RawMessage {
	return json.RawMessage(value)
}

func TestBuildBlockerReportCapsRelationFanoutAndParsesGroupedRelations(t *testing.T) {
	reader := &fakeProjectWorkflowReader{responses: map[string][]json.RawMessage{
		"/projects/project-1/work-items/issue-1/relations/": {rawJSON(`{"data":{
			"blocked_by": [
				"blocker-0",
				{"issue": {"id": "blocker-2", "name": "Second blocker", "sequence_id": 2, "project_identifier": "WEB"}},
				{"id": "blocker-1", "name": "First blocker", "sequence_id": 1, "project__identifier": "WEB"}
			],
			"blocking": [{"related_issue": {"id": "downstream-1", "name": "Downstream", "identifier": "WEB-9"}}],
			"relates_to": [{"id": "ignored"}]
		}}`)},
		"/projects/project-1/work-items/issue-2/relations/": {rawJSON(`{
			"blocked_by": [],
			"blocking": [],
			"duplicate": [{"id": "ignored-too"}]
		}`)},
	}}
	issues := []json.RawMessage{
		rawJSON(`{"id":"issue-1","name":"First","sequence_id":10,"project_identifier":"WEB"}`),
		rawJSON(`{"id":"issue-2","name":"Second","identifier":"WEB-11"}`),
		rawJSON(`{"id":"issue-3","name":"Must not be fetched"}`),
	}

	report, err := buildBlockerReport(context.Background(), reader, "project-1", issues, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.calls) != 2 {
		t.Fatalf("relation GET count = %d, want 2", len(reader.calls))
	}
	for _, call := range reader.calls {
		if !strings.HasSuffix(call.path, "/relations/") {
			t.Errorf("unexpected non-relation GET: %s", call.path)
		}
	}
	if !report.Truncated || report.ScannedIssues != 2 || report.AvailableIssues != 3 {
		t.Fatalf("fan-out metadata = truncated:%v scanned:%d available:%d, want true/2/3",
			report.Truncated, report.ScannedIssues, report.AvailableIssues)
	}
	if report.BlockedCount != 3 || report.BlockingCount != 1 || report.RelationEdges != 4 {
		t.Fatalf("relation counts = blocked:%d blocking:%d edges:%d, want 3/1/4",
			report.BlockedCount, report.BlockingCount, report.RelationEdges)
	}
	if len(report.Entries) != 1 {
		t.Fatalf("entries = %d, want only one work item with blocking relations", len(report.Entries))
	}
	entry := report.Entries[0]
	if entry.Issue.Identifier != "WEB-10" {
		t.Errorf("issue identity = %#v, want identifier WEB-10", entry.Issue)
	}
	if got := []string{entry.BlockedBy[0].ID, entry.BlockedBy[1].ID, entry.BlockedBy[2].ID}; !reflect.DeepEqual(got, []string{"blocker-0", "blocker-1", "blocker-2"}) {
		t.Errorf("sorted blocked_by ids = %v", got)
	}
	if entry.BlockedBy[1].Identifier != "WEB-1" || entry.Blocking[0].Identifier != "WEB-9" {
		t.Errorf("related identities not normalized: %#v", entry)
	}
}

func TestReadProjectWorkflowPagesUsesNoCacheReaderForEveryPage(t *testing.T) {
	const path = "/projects/project-1/work-items/"
	reader := &fakeProjectWorkflowReader{responses: map[string][]json.RawMessage{
		path: {
			rawJSON(`{"results":[{"id":"one"}],"next_cursor":"cursor-2"}`),
			rawJSON(`{"results":[{"id":"two"}],"next_cursor":null}`),
		},
	}}
	items, err := readProjectWorkflowPages(context.Background(), reader, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if len(reader.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(reader.calls))
	}
	if got := reader.calls[0].params; !reflect.DeepEqual(got, map[string]string{"per_page": "100"}) {
		t.Errorf("first-page params = %#v", got)
	}
	if got := reader.calls[1].params; !reflect.DeepEqual(got, map[string]string{"cursor": "cursor-2", "per_page": "100"}) {
		t.Errorf("second-page params = %#v", got)
	}
}

func TestBuildProjectHealthAggregatesLiveMetrics(t *testing.T) {
	reader := &fakeProjectWorkflowReader{responses: map[string][]json.RawMessage{
		"/projects/project-1/summary/": {rawJSON(`{
			"id":"project-1",
			"name":"Website",
			"identifier":"WEB",
			"raw_field_that_must_not_leak":"secret"
		}`)},
		"/projects/project-1/states/": {rawJSON(`[
			{"id":"started","name":"In Progress","group":"started"},
			{"id":"done","name":"Done","group":"completed"}
		]`)},
		"/projects/project-1/work-items/": {rawJSON(`[
			{"id":"one","name":"One","state":"started","priority":"high"},
			{"id":"two","name":"Two","state":"done","priority":"high","completed_at":"2026-07-24T00:00:00Z"},
			{"id":"three","name":"Three","state":{"id":"done","name":"Done","group":"completed"},"priority":"low"}
		]`)},
		"/projects/project-1/work-items/one/relations/": {rawJSON(`{"blocked_by":[{"id":"external","name":"External blocker"}],"blocking":[]}`)},
		"/projects/project-1/work-items/two/relations/": {rawJSON(`{"blocked_by":[],"blocking":[{"id":"three","name":"Three"}]}`)},
	}}

	report, err := buildProjectHealth(context.Background(), reader, "project-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if report.Project.Name != "Website" || report.Project.Identifier != "WEB" {
		t.Errorf("project identity = %#v", report.Project)
	}
	if report.IssueCount != 3 || report.CompletedCount != 2 || report.CompletionPercent != 200.0/3.0 {
		t.Errorf("completion metrics = issues:%d completed:%d percent:%v",
			report.IssueCount, report.CompletedCount, report.CompletionPercent)
	}
	if !reflect.DeepEqual(report.ByState, map[string]int{"Done": 2, "In Progress": 1}) {
		t.Errorf("by_state = %#v", report.ByState)
	}
	if !reflect.DeepEqual(report.ByStateGroup, map[string]int{"completed": 2, "started": 1}) {
		t.Errorf("by_state_group = %#v", report.ByStateGroup)
	}
	if !reflect.DeepEqual(report.ByPriority, map[string]int{"high": 2, "low": 1}) {
		t.Errorf("by_priority = %#v", report.ByPriority)
	}
	if report.Blockers.ScannedIssues != 2 || report.Blockers.BlockedCount != 1 ||
		report.Blockers.BlockingCount != 1 || report.Blockers.RelationEdges != 2 {
		t.Errorf("blocker metrics = %#v", report.Blockers)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "raw_field_that_must_not_leak") {
		t.Errorf("project-health leaked raw summary: %s", encoded)
	}
}

func TestProjectWorkflowCommandsAreStaticReadOnlyAndRequirePositiveLimit(t *testing.T) {
	flags := &rootFlags{}
	for _, cmd := range []*cobra.Command{
		newWorkflowProjectHealthCmd(flags),
		newWorkflowBlockersCmd(flags),
	} {
		if got := cmd.Annotations["mcp:read-only"]; got != "true" {
			t.Errorf("%s mcp:read-only = %q, want true", cmd.Name(), got)
		}
		cmd.SetArgs([]string{"project-1", "--limit", "0"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--limit must be greater than zero") {
			t.Errorf("%s limit validation error = %v", cmd.Name(), err)
		}
	}
}
