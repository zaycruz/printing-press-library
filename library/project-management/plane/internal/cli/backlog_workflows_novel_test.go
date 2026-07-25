package cli

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type backlogFake struct {
	reads      []json.RawMessage
	getPaths   []string
	getParams  []map[string]string
	patchPaths []string
	patchBody  []map[string]any
	events     []string
	getErrAt   int
}

func (f *backlogFake) GetNoCache(_ context.Context, path string, params map[string]string) (json.RawMessage, error) {
	f.getPaths = append(f.getPaths, path)
	f.getParams = append(f.getParams, params)
	f.events = append(f.events, "GET "+path)
	if f.getErrAt > 0 && len(f.getPaths) == f.getErrAt {
		return nil, errors.New("live read failed")
	}
	if len(f.reads) == 0 {
		return nil, errors.New("unexpected live read")
	}
	raw := f.reads[0]
	f.reads = f.reads[1:]
	return raw, nil
}

func (f *backlogFake) Patch(_ context.Context, path string, body any) (json.RawMessage, int, error) {
	f.patchPaths = append(f.patchPaths, path)
	f.patchBody = append(f.patchBody, cloneMap(body.(map[string]any)))
	f.events = append(f.events, "PATCH "+path)
	return json.RawMessage(`{}`), 200, nil
}

func TestWorkflowBacklogAnnotationsClassifyOnlyTriageAsReadOnly(t *testing.T) {
	flags := &rootFlags{}
	triage := newWorkflowBacklogTriageCmd(flags)
	apply := newWorkflowBacklogApplyCmd(flags)
	if triage.Annotations["mcp:read-only"] != "true" {
		t.Fatal("backlog-triage must carry static mcp:read-only annotation")
	}
	if _, ok := apply.Annotations["mcp:read-only"]; ok {
		t.Fatal("backlog-apply mutates and must not carry mcp:read-only")
	}
}

func TestTriageBacklogUsesLiveReadExcludesEveryCycledRepresentationAndNeverMutates(t *testing.T) {
	fake := &backlogFake{reads: []json.RawMessage{json.RawMessage(`{
		"results": [
			{"id":"1","project_identifier":"PLN","sequence_id":7,"name":"candidate","cycle":null,"priority":"high","state":{"id":"state-1"},"assignees":[{"id":"user-1"}],"labels":["label-1"],"point":3},
			{"id":"2","name":"scalar cycle","cycle":"cycle-1"},
			{"id":"3","name":"object cycle","cycle":{"id":"cycle-2"}},
			{"id":"4","name":"cycle id","cycle":null,"cycle_id":"cycle-3"},
			{"id":"5","name":"empty forms","cycle":{},"cycle_id":""}
		]
	}`)}}

	got, err := triageBacklog(context.Background(), fake, "project-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.patchPaths) != 0 {
		t.Fatalf("read-only triage PATCHed %d times", len(fake.patchPaths))
	}
	if len(fake.getPaths) != 1 || fake.getPaths[0] != "/projects/project-1/issues/" {
		t.Fatalf("live reads = %#v", fake.getPaths)
	}
	if got.Count != 2 || len(got.Candidates) != 2 {
		t.Fatalf("candidates = %#v, want only the two uncycled items", got.Candidates)
	}
	if got.Candidates[0].Identifier != "PLN-7" || got.Candidates[0].State != "state-1" ||
		got.Candidates[0].AssigneeCount != 1 || got.Candidates[0].LabelCount != 1 {
		t.Fatalf("candidate summary = %#v", got.Candidates[0])
	}
	if got.Candidates[1].ID != "5" {
		t.Fatalf("empty server cycle forms should be backlog: %#v", got.Candidates[1])
	}
}

func TestApplyBacklogDryRunLivePreviewsEveryTargetBeforeReturningAndNeverPatches(t *testing.T) {
	fake := &backlogFake{reads: []json.RawMessage{
		json.RawMessage(`{"id":"issue-1","priority":"low"}`),
		json.RawMessage(`{"id":"issue-2","priority":"medium"}`),
	}}
	desired := map[string]any{"priority": "high"}
	got, err := applyBacklog(context.Background(), fake, "project-1", []string{"issue-1", "issue-2"}, desired, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.patchPaths) != 0 {
		t.Fatalf("dry run PATCHed %d times", len(fake.patchPaths))
	}
	if len(fake.getPaths) != 2 || !got.DryRun || len(got.Items) != 2 {
		t.Fatalf("result=%#v reads=%#v", got, fake.getPaths)
	}
	if got.Items[0].Before["priority"] != "low" || got.Items[0].Verified {
		t.Fatalf("preview = %#v", got.Items[0])
	}
}

func TestApplyBacklogCompletesAllLivePreflightBeforeFirstPatch(t *testing.T) {
	fake := &backlogFake{
		reads:    []json.RawMessage{json.RawMessage(`{"id":"issue-1"}`)},
		getErrAt: 2,
	}
	_, err := applyBacklog(context.Background(), fake, "project-1", []string{"issue-1", "issue-2"}, map[string]any{"priority": "high"}, false)
	if err == nil || !strings.Contains(err.Error(), "live preview for work item issue-2") {
		t.Fatalf("error = %v", err)
	}
	if len(fake.patchPaths) != 0 {
		t.Fatalf("preflight failure must prevent all PATCHes, got %#v", fake.patchPaths)
	}
}

func TestApplyBacklogPatchesThenLiveReadsAndNormalizesServerRepresentations(t *testing.T) {
	fake := &backlogFake{reads: []json.RawMessage{
		json.RawMessage(`{"id":"issue-1","state":"old"}`),
		json.RawMessage(`{"id":"issue-2","state":"old"}`),
		json.RawMessage(`{"id":"issue-1","state":{"id":"state-1"},"assignees":[{"id":"user-2"},{"id":"user-1"}],"labels":["label-1"],"point":3.0,"target_date":null}`),
		json.RawMessage(`{"data":{"id":"issue-2","state":"state-1","assignees":["user-1","user-2"],"labels":[{"id":"label-1"}],"point":3,"target_date":""}}`),
	}}
	desired := map[string]any{
		"state":       "state-1",
		"assignees":   []string{"user-1", "user-2"},
		"labels":      []string{"label-1"},
		"point":       3,
		"target_date": nil,
	}
	got, err := applyBacklog(context.Background(), fake, "project-1", []string{"issue-1", "issue-2"}, desired, false)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"GET /projects/project-1/issues/issue-1/",
		"GET /projects/project-1/issues/issue-2/",
		"PATCH /projects/project-1/issues/issue-1/",
		"GET /projects/project-1/issues/issue-1/",
		"PATCH /projects/project-1/issues/issue-2/",
		"GET /projects/project-1/issues/issue-2/",
	}
	if !reflect.DeepEqual(fake.events, wantEvents) {
		t.Fatalf("call order = %#v, want %#v", fake.events, wantEvents)
	}
	for _, item := range got.Items {
		if !item.Verified || item.After == nil {
			t.Fatalf("unverified result: %#v", item)
		}
	}
}

func TestVerifyBacklogMutationAcceptsNullAndEmptyCollectionForms(t *testing.T) {
	desired := map[string]any{
		"state":       nil,
		"assignees":   []string{},
		"labels":      []string{},
		"point":       0,
		"target_date": nil,
	}
	actual := map[string]any{
		"state":       map[string]any{},
		"assignees":   nil,
		"labels":      []any{},
		"point":       json.Number("0.0"),
		"target_date": "",
	}
	if err := verifyBacklogMutation(desired, actual); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBacklogMutationFailsClosedOnReadbackMismatch(t *testing.T) {
	err := verifyBacklogMutation(
		map[string]any{"state": "state-1", "point": 3},
		map[string]any{"state": map[string]any{"id": "state-2"}, "point": 5.0},
	)
	if err == nil || !strings.Contains(err.Error(), "state") || !strings.Contains(err.Error(), "point") {
		t.Fatalf("error = %v", err)
	}
}
