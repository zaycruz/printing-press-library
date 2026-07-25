package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeCycleWorkflowAPI struct {
	gets      []string
	posts     []fakeCyclePost
	responses map[string][]json.RawMessage
	getErr    map[string]error
	postErr   error
}

type fakeCyclePost struct {
	path string
	body any
}

func (f *fakeCycleWorkflowAPI) GetNoCache(_ context.Context, path string, params map[string]string) (json.RawMessage, error) {
	key := path
	if cursor := params["cursor"]; cursor != "" {
		key += "?cursor=" + cursor
	}
	f.gets = append(f.gets, key)
	if err := f.getErr[key]; err != nil {
		return nil, err
	}
	queue := f.responses[key]
	if len(queue) == 0 {
		return nil, fmt.Errorf("unexpected live GET %s", key)
	}
	out := queue[0]
	f.responses[key] = queue[1:]
	return out, nil
}

func (f *fakeCycleWorkflowAPI) Post(_ context.Context, path string, body any) (json.RawMessage, int, error) {
	f.posts = append(f.posts, fakeCyclePost{path: path, body: body})
	if f.postErr != nil {
		return nil, 500, f.postErr
	}
	return json.RawMessage(`{"ok":true}`), 200, nil
}

func TestCyclePlanFromBacklogExcludesMembershipAcrossEveryCycleAndPostsOnce(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/target/": {
				json.RawMessage(`{"id":"target","name":"Next"}`),
			},
			"/projects/project/cycles/": {
				json.RawMessage(`{"results":[{"id":"past"},{"id":"target"}],"next_cursor":null}`),
			},
			"/projects/project/cycles/past/cycle-issues/": {
				json.RawMessage(`[{"id":"membership-past","issue":"assigned-past"}]`),
			},
			"/projects/project/cycles/target/cycle-issues/": {
				json.RawMessage(`[{"id":"membership-target","issue":"assigned-target"}]`),
				json.RawMessage(`[
					{"id":"membership-target","issue":"assigned-target"},
					{"id":"membership-free-1","issue":"free-1"},
					{"id":"membership-free-2","issue":{"id":"free-2"}}
				]`),
			},
			"/projects/project/issues/": {
				json.RawMessage(`[
					{"id":"assigned-past"},
					{"id":"free-1"},
					{"id":"assigned-target"},
					{"id":"free-2"},
					{"id":"free-over-limit"}
				]`),
			},
			"/projects/project/issues/free-1/": {json.RawMessage(`{"id":"free-1"}`)},
			"/projects/project/issues/free-2/": {json.RawMessage(`{"id":"free-2"}`)},
		},
		getErr: map[string]error{},
	}

	result, err := runCyclePlan(context.Background(), api, cyclePlanOptions{
		ProjectID:   "project",
		TargetCycle: "target",
		FromBacklog: true,
		Limit:       2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result["selected_issue_ids"], []string{"free-1", "free-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected_issue_ids = %#v, want %#v", got, want)
	}
	if got := result["already_cycled_count"]; got != 2 {
		t.Fatalf("already_cycled_count = %#v, want 2", got)
	}
	if len(api.posts) != 1 {
		t.Fatalf("POST count = %d, want exactly 1", len(api.posts))
	}
	if api.posts[0].path != "/projects/project/cycles/target/cycle-issues/" {
		t.Fatalf("POST path = %q", api.posts[0].path)
	}
	body, ok := api.posts[0].body.(map[string]any)
	if !ok || !reflect.DeepEqual(body["issues"], []string{"free-1", "free-2"}) {
		t.Fatalf("atomic POST body = %#v", api.posts[0].body)
	}
	for _, requiredLiveRead := range []string{
		"/projects/project/cycles/",
		"/projects/project/cycles/past/cycle-issues/",
		"/projects/project/cycles/target/cycle-issues/",
		"/projects/project/issues/free-1/",
		"/projects/project/issues/free-2/",
	} {
		if !containsCycleCall(api.gets, requiredLiveRead) {
			t.Errorf("missing live read %s; calls: %v", requiredLiveRead, api.gets)
		}
	}
}

func TestCyclePlanDryRunStillLivePreflightsAndNeverPosts(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/target/": {json.RawMessage(`{"id":"target"}`)},
			"/projects/project/issues/a/":      {json.RawMessage(`{"id":"a"}`)},
			"/projects/project/issues/b/":      {json.RawMessage(`{"id":"b"}`)},
		},
		getErr: map[string]error{},
	}

	result, err := runCyclePlan(context.Background(), api, cyclePlanOptions{
		ProjectID:   "project",
		TargetCycle: "target",
		IssueIDs:    []string{"a", "b"},
		Limit:       2,
		DryRun:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["dry_run"] != true || result["live_preflight_passed"] != true {
		t.Fatalf("dry-run result = %#v", result)
	}
	if len(api.posts) != 0 {
		t.Fatalf("dry-run made %d POST(s), want none", len(api.posts))
	}
	if got, want := api.gets, []string{
		"/projects/project/cycles/target/",
		"/projects/project/issues/a/",
		"/projects/project/issues/b/",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("live dry-run reads = %v, want %v", got, want)
	}
}

func TestCyclePlanPreflightFailurePreventsAtomicMutation(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/target/": {json.RawMessage(`{"id":"target"}`)},
			"/projects/project/issues/a/":      {json.RawMessage(`{"id":"a"}`)},
		},
		getErr: map[string]error{
			"/projects/project/issues/b/": errors.New("404"),
		},
	}

	_, err := runCyclePlan(context.Background(), api, cyclePlanOptions{
		ProjectID:   "project",
		TargetCycle: "target",
		IssueIDs:    []string{"a", "b"},
		Limit:       2,
	})
	if err == nil || !strings.Contains(err.Error(), "live preflight for work item b") {
		t.Fatalf("error = %v, want b preflight failure", err)
	}
	if len(api.posts) != 0 {
		t.Fatalf("preflight failure made %d POST(s), want none", len(api.posts))
	}
}

func TestCyclePlanPaginatesCycleListBeforeSelectingBacklog(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/target/": {json.RawMessage(`{"id":"target"}`)},
			"/projects/project/cycles/": {
				json.RawMessage(`{"results":[{"id":"first"}],"next_cursor":"page-2"}`),
			},
			"/projects/project/cycles/?cursor=page-2": {
				json.RawMessage(`{"results":[{"id":"second"}],"next_cursor":null}`),
			},
			"/projects/project/cycles/first/cycle-issues/": {
				json.RawMessage(`[{"id":"membership-first","issue":"assigned-first"}]`),
			},
			"/projects/project/cycles/second/cycle-issues/": {
				json.RawMessage(`[{"id":"membership-second","issue":"assigned-second"}]`),
			},
			"/projects/project/issues/": {
				json.RawMessage(`[{"id":"assigned-second"},{"id":"free"}]`),
			},
			"/projects/project/issues/free/": {json.RawMessage(`{"id":"free"}`)},
		},
		getErr: map[string]error{},
	}

	result, err := runCyclePlan(context.Background(), api, cyclePlanOptions{
		ProjectID:   "project",
		TargetCycle: "target",
		FromBacklog: true,
		Limit:       1,
		DryRun:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := result["selected_issue_ids"], []string{"free"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("selected_issue_ids = %#v, want %#v", got, want)
	}
	if !containsCycleCall(api.gets, "/projects/project/cycles/?cursor=page-2") {
		t.Fatalf("second cycle page not read: %v", api.gets)
	}
}

func TestCycleHealthAggregatesLiveMetricsAndIsStaticallyReadOnly(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/cycle/": {
				json.RawMessage(`{"id":"cycle","name":"Sprint"}`),
			},
			"/projects/project/cycles/cycle/cycle-issues/": {
				json.RawMessage(`[
					{"id":"membership-done","issue":"done"},
					{"id":"membership-doing","issue":{"id":"doing"}},
					{"id":"membership-todo","issue_id":"todo"}
				]`),
			},
			"/projects/project/issues/done/": {
				json.RawMessage(`{"id":"done","priority":"high","completed_at":"2026-07-01T00:00:00Z","assignees":["u"]}`),
			},
			"/projects/project/issues/doing/": {
				json.RawMessage(`{"id":"doing","priority":"high","state_detail":{"group":"started"},"assignees":[]}`),
			},
			"/projects/project/issues/todo/": {
				json.RawMessage(`{"id":"todo","state":{"group":"unstarted"}}`),
			},
		},
		getErr: map[string]error{},
	}

	result, err := runCycleHealth(context.Background(), api, "project", "cycle", 10)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"total":      3,
		"completed":  1,
		"incomplete": 2,
		"unassigned": 2,
		"source":     "live",
	} {
		if got := result[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if got := result["completion_pct"]; got != float64(100)/3 {
		t.Errorf("completion_pct = %#v", got)
	}
	cmd := newWorkflowCycleHealthCmd(&rootFlags{})
	if got := cmd.Annotations["mcp:read-only"]; got != "true" {
		t.Fatalf("cycle-health mcp:read-only = %q, want true", got)
	}
	for _, mutator := range []*cobraCommandAnnotation{
		{name: "cycle-plan", annotations: newWorkflowCyclePlanCmd(&rootFlags{}).Annotations},
		{name: "cycle-rollover", annotations: newWorkflowCycleRolloverCmd(&rootFlags{}).Annotations},
	} {
		if _, exists := mutator.annotations["mcp:read-only"]; exists {
			t.Errorf("%s is incorrectly annotated mcp:read-only", mutator.name)
		}
	}
}

func TestCycleHealthLimitFailsClosedBeforeIssueFanOut(t *testing.T) {
	api := &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/cycle/": {
				json.RawMessage(`{"id":"cycle"}`),
			},
			"/projects/project/cycles/cycle/cycle-issues/": {
				json.RawMessage(`[
					{"id":"membership-a","issue":"a"},
					{"id":"membership-b","issue":"b"}
				]`),
			},
		},
		getErr: map[string]error{},
	}

	_, err := runCycleHealth(context.Background(), api, "project", "cycle", 1)
	if err == nil || !strings.Contains(err.Error(), "exceeding --limit 1") {
		t.Fatalf("error = %v, want limit failure", err)
	}
	for _, call := range api.gets {
		if strings.Contains(call, "/issues/") {
			t.Fatalf("limit failure still fanned out to issue detail: %v", api.gets)
		}
	}
}

type cobraCommandAnnotation struct {
	name        string
	annotations map[string]string
}

func TestCycleRolloverDryRunPreviewsBothCyclesAndMembershipsWithoutPost(t *testing.T) {
	api := newRolloverFake(false)
	result, err := runCycleRollover(context.Background(), api, cycleRolloverOptions{
		ProjectID:   "project",
		SourceCycle: "source",
		TargetCycle: "target",
		Limit:       10,
		DryRun:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.posts) != 0 {
		t.Fatalf("dry-run made %d POST(s), want none", len(api.posts))
	}
	if got, want := result["movable_issue_ids"], []string{"move"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("movable_issue_ids = %#v, want %#v", got, want)
	}
	if got := len(api.gets); got != 6 {
		t.Fatalf("dry-run live GET count = %d, want 6 (%v)", got, api.gets)
	}
}

func TestCycleRolloverPostsOnceThenFreshReadsAndVerifiesBothSides(t *testing.T) {
	api := newRolloverFake(true)
	result, err := runCycleRollover(context.Background(), api, cycleRolloverOptions{
		ProjectID:   "project",
		SourceCycle: "source",
		TargetCycle: "target",
		Limit:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.posts) != 1 {
		t.Fatalf("POST count = %d, want 1", len(api.posts))
	}
	if got, want := api.posts[0].path, "/projects/project/cycles/source/transfer-issues/"; got != want {
		t.Fatalf("POST path = %q, want %q", got, want)
	}
	body := api.posts[0].body.(map[string]any)
	if body["new_cycle_id"] != "target" {
		t.Fatalf("POST body = %#v", body)
	}
	if result["verified"] != true {
		t.Fatalf("result = %#v", result)
	}
	if got := countCycleCall(api.gets, "/projects/project/cycles/source/"); got != 2 {
		t.Fatalf("source detail live reads = %d, want preview + readback", got)
	}
	if got := countCycleCall(api.gets, "/projects/project/cycles/target/cycle-issues/"); got != 2 {
		t.Fatalf("target membership live reads = %d, want preview + readback", got)
	}
}

func newRolloverFake(withReadback bool) *fakeCycleWorkflowAPI {
	sourceDetails := []json.RawMessage{json.RawMessage(`{"id":"source"}`)}
	targetDetails := []json.RawMessage{json.RawMessage(`{"id":"target"}`)}
	sourceMembers := []json.RawMessage{
		json.RawMessage(`[
			{"id":"membership-done","issue":"done"},
			{"id":"membership-move","issue":"move"}
		]`),
	}
	targetMembers := []json.RawMessage{json.RawMessage(`[{"id":"membership-existing","issue":"existing"}]`)}
	if withReadback {
		sourceDetails = append(sourceDetails, json.RawMessage(`{"id":"source"}`))
		targetDetails = append(targetDetails, json.RawMessage(`{"id":"target"}`))
		sourceMembers = append(sourceMembers, json.RawMessage(`[{"id":"membership-done","issue":"done"}]`))
		targetMembers = append(targetMembers, json.RawMessage(`[
			{"id":"membership-existing","issue":"existing"},
			{"id":"membership-move","issue_id":"move"}
		]`))
	}
	return &fakeCycleWorkflowAPI{
		responses: map[string][]json.RawMessage{
			"/projects/project/cycles/source/":              sourceDetails,
			"/projects/project/cycles/target/":              targetDetails,
			"/projects/project/cycles/source/cycle-issues/": sourceMembers,
			"/projects/project/cycles/target/cycle-issues/": targetMembers,
			"/projects/project/issues/done/": {
				json.RawMessage(`{"id":"done","state_detail":{"group":"completed"}}`),
			},
			"/projects/project/issues/move/": {
				json.RawMessage(`{"id":"move","state_detail":{"group":"started"}}`),
			},
		},
		getErr: map[string]error{},
	}
}

func containsCycleCall(calls []string, want string) bool {
	return countCycleCall(calls, want) > 0
}

func countCycleCall(calls []string, want string) int {
	count := 0
	for _, call := range calls {
		if call == want {
			count++
		}
	}
	return count
}
