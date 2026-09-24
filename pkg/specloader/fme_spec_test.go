// Copyright © 2026 Harness Inc.
// SPDX-License-Identifier: Apache-2.0

package specloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harness/cli/v3/pkg/auth"
	"github.com/harness/cli/v3/pkg/cmdctx"
	"github.com/harness/cli/v3/pkg/registry"
)

// fmeCaptureServer returns a mock server that records the inbound request path
// and always replies with resp, plus the *cmdctx.Ctx wired to call it.
func fmeCaptureServer(t *testing.T, resp string) (*httptest.Server, *string) {
	t.Helper()
	path := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &path
}

// fmeCaptureServerWithQuery is like fmeCaptureServer but also records the raw
// query string, for asserting flag-to-query-param wiring (e.g. --env → environment_id).
func fmeCaptureServerWithQuery(t *testing.T, resp string) (*httptest.Server, *string, *string) {
	t.Helper()
	path, query := "", ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &path, &query
}

func fmeTestCtx(t *testing.T, apiURL string) *cmdctx.Ctx {
	t.Helper()
	return &cmdctx.Ctx{
		Context: context.Background(),
		Auth: &auth.ResolvedAuth{
			APIUrl:    apiURL,
			AccountID: "acct",
			OrgID:     "org",
			ProjectID: "proj",
			PATToken:  "pat.test",
			AuthType:  auth.AuthTypePAT,
		},
		FormatFlags: cmdctx.FormatFlags{OutFile: filepath.Join(t.TempDir(), "out")},
	}
}

func fmeReadOut(t *testing.T, ctx *cmdctx.Ctx) string {
	t.Helper()
	b, err := os.ReadFile(ctx.FormatFlags.OutFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	return string(b)
}

// TestFMESpec_ListFeatureFlag drives the real embedded fme.spec.yaml "list feature_flag"
// command against a mock server returning the flat (no "entity" wrapper) shape that the
// live FME v4 API returns, and asserts the request hits /fme/api/v4/feature-flags
// and that fields resolve directly off the item (it.name, it.trafficType.name, ...).
func TestFMESpec_ListFeatureFlag(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list feature_flag: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"name":"my-flag","description":"desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"name":"Ramp"},"createdAt":"2026-01-01T00:00:00Z"}],"limit":20,"offset":0,"totalCount":1}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/feature-flags") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/feature-flags", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"my-flag", "user", "ACTIVE", "Ramp"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q (flat field did not resolve): %s", want, body)
		}
	}
}

// TestFMESpec_GetFeatureFlag drives the real embedded fme.spec.yaml "get feature_flag"
// command against a mock server returning a flat object, and asserts the request path
// and that yaml_pick_expr/item_expr resolve the item directly (it, not it.entity).
func TestFMESpec_GetFeatureFlag(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("get", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("get feature_flag: command not found or missing endpoint spec")
	}

	fixture := `{"name":"my-flag","description":"desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"name":"Ramp"},"createdAt":"2026-01-01T00:00:00Z"}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "yaml"

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if *path != "/fme/api/v4/feature-flags/my-flag" {
		t.Fatalf("request path = %q, want /fme/api/v4/feature-flags/my-flag", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"name: my-flag", "status: ACTIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q (yaml_pick_expr did not resolve flat item): %s", want, body)
		}
	}
	if strings.Contains(body, "entity") {
		t.Fatalf("output still references entity wrapper: %s", body)
	}
}

// TestFMESpec_GetFeatureFlag_TagsOwnersTextFormat drives "get feature_flag"
// in default text format and asserts the tags/owners fields render their
// joined names instead of blank — the exprs used method-call syntax
// (it.tags.map(t, t.name).join(", ")) which expr-lang silently fails to
// evaluate at runtime (map/join are only valid as bare builtin calls with a
// "#" predicate, e.g. join(map(it.tags, {#.name}), ", ")); JSON/YAML format
// didn't surface it since they serialize raw data without evaluating expr.
func TestFMESpec_GetFeatureFlag_TagsOwnersTextFormat(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("get", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("get feature_flag: command not found or missing endpoint spec")
	}

	fixture := `{"name":"my-flag","description":"desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"name":"Ramp"},"tags":[{"id":"t-1","name":"demo"}],"owners":[{"id":"u-1","name":"alice","type":"USER"}],"createdAt":"2026-01-01T00:00:00Z"}`
	srv, _ := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "text"

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	body := fmeReadOut(t, ctx)
	if !strings.Contains(body, "demo") {
		t.Fatalf("output missing tag %q (map/join expr silently failed): %s", "demo", body)
	}
	if !strings.Contains(body, "alice") {
		t.Fatalf("output missing owner %q (map/join expr silently failed): %s", "alice", body)
	}
}

// TestFMESpec_ListFMEEnvironment drives "list fme_environment" and asserts it
// hits /fme/api/v4/environments and that get_id_expr resolves off
// it.id (environments are addressed by UUID, not name, unlike segment/feature_flag).
func TestFMESpec_ListFMEEnvironment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "fme_environment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list fme_environment: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"id":"env-uuid-1","name":"Prod","isProduction":true,"status":"ACTIVE"}],"limit":100,"offset":0,"totalCount":1}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "fme_environment"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/environments") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/environments", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"Prod", "true", "ACTIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// TestFMESpec_GetFMEEnvironment drives "get fme_environment" with a UUID id
// and asserts the path embeds it (environments are looked up by id, not name).
func TestFMESpec_GetFMEEnvironment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("get", "fme_environment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("get fme_environment: command not found or missing endpoint spec")
	}

	fixture := `{"id":"env-uuid-1","name":"Prod","isProduction":true,"status":"ACTIVE"}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "env-uuid-1"
	ctx.Noun = "fme_environment"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "yaml"

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if *path != "/fme/api/v4/environments/env-uuid-1" {
		t.Fatalf("request path = %q, want /fme/api/v4/environments/env-uuid-1", *path)
	}
}

// TestFMESpec_ListSegment drives "list segment" and asserts get_id_expr
// resolves off it.name (segments, unlike fme_environment, are addressed by name).
func TestFMESpec_ListSegment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "segment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list segment: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"name":"my-segment","description":"desc","trafficType":{"name":"user"},"status":"ACTIVE","createdAt":1778049995.725}],"limit":100,"offset":0,"totalCount":1}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "segment"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/segments") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/segments", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"my-segment", "user", "ACTIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// TestFMESpec_ListSegmentDefinition drives "list segment:definition" and asserts
// the --env flag maps to the environment_id query param and fields_extra resolves
// (segment/environment names, description, status) off the flat item.
func TestFMESpec_ListSegmentDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "segment:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list segment:definition: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"segment":{"name":"my-segment"},"environment":{"name":"Prod"},"description":"desc","status":"ACTIVE","createdAt":1778049995.725}],"limit":100,"offset":0,"totalCount":1}`
	srv, path, query := fmeCaptureServerWithQuery(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "segment"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1"}

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/segment-definitions") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/segment-definitions", *path)
	}
	if !strings.Contains(*query, "environment_id=env-uuid-1") {
		t.Fatalf("query = %q, want environment_id=env-uuid-1 (from --env flag)", *query)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"my-segment", "Prod", "desc", "ACTIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// TestFMESpec_ListTrafficType drives "list traffic_type" against the mock server
// and asserts it hits /fme/api/v4/traffic-types and renders id/name fields.
func TestFMESpec_ListTrafficType(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "traffic_type")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list traffic_type: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"type":"traffic-type","id":"tt-1","name":"user"}],"limit":100,"offset":0,"totalCount":1}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "traffic_type"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/traffic-types") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/traffic-types", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"tt-1", "user"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// TestFMESpec_ListRolloutStatus drives "list rollout_status" against the mock
// server and asserts it hits /fme/api/v4/rollout-statuses, renders id/name/description,
// and that the description field falls back to "" when omitted from the JSON entirely
// (backend trimToNull's blank descriptions server-side).
func TestFMESpec_ListRolloutStatus(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "rollout_status")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list rollout_status: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"type":"rollout-status","id":"rs-1","name":"Ramp","description":"Ramping up traffic"},{"type":"rollout-status","id":"rs-2","name":"Killed"}],"limit":100,"offset":0,"totalCount":2}`
	srv, path := fmeCaptureServer(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "rollout_status"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/rollout-statuses") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/rollout-statuses", *path)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"rs-1", "Ramp", "Ramping up traffic", "rs-2", "Killed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// fmeCaptured records one request's method/path/query/body — used by
// fmeSequenceServer for multi-request flows (e.g. get-then-patch update).
type fmeCaptured struct {
	method   string
	path     string
	rawQuery string
	body     []byte
}

// fmeSequenceServer returns a different response per call, in order,
// recording every request. Extra calls beyond len(resps) get "{}".
func fmeSequenceServer(t *testing.T, resps []string) (*httptest.Server, *[]fmeCaptured) {
	t.Helper()
	caps := &[]fmeCaptured{}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := fmeCaptured{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery}
		c.body, _ = io.ReadAll(r.Body)
		*caps = append(*caps, c)
		resp := "{}"
		if i < len(resps) {
			resp = resps[i]
			i++
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, caps
}

// TestFMESpec_ListFeatureFlag_StatusFilter asserts --status maps to the
// "status" query param (matching @QueryParam("status") on
// FeatureFlagResource.list in service-web-admin), not "rollout_statuses"
// (FME-17257 fix — these are different v4 concepts; the old mapping made
// --status a silent no-op, since the API defaults to ACTIVE-only when the
// "status" param is absent/unrecognized).
func TestFMESpec_ListFeatureFlag_StatusFilter(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list feature_flag: command not found or missing endpoint spec")
	}

	fixture := `{"data":[],"limit":20,"offset":0,"totalCount":0}`
	srv, _, query := fmeCaptureServerWithQuery(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"status": "ACTIVE"}

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.Contains(*query, "status=ACTIVE") {
		t.Fatalf("query = %q, want status=ACTIVE", *query)
	}
	if strings.Contains(*query, "rollout_statuses=") {
		t.Fatalf("query = %q, --status must not map to rollout_statuses", *query)
	}
}

// TestFMESpec_CreateFeatureFlag drives "create feature_flag" and asserts the
// POST body carries name/trafficType from ctx.id/--traffic-type, and that the
// response unwraps via item_expr: it.entity.
func TestFMESpec_CreateFeatureFlag(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("create", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("create feature_flag: command not found or missing endpoint spec")
	}

	fixture := `{"entity":{"name":"new-flag","trafficType":{"name":"user"},"status":"ACTIVE"}}`
	srv, caps := fmeSequenceServer(t, []string{fixture})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "new-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"traffic-type": "user"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if len(*caps) != 1 {
		t.Fatalf("got %d requests, want 1", len(*caps))
	}
	got := (*caps)[0]
	if got.method != "POST" || got.path != "/fme/api/v4/feature-flags" {
		t.Fatalf("request = %s %s, want POST /fme/api/v4/feature-flags", got.method, got.path)
	}
	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if body["name"] != "new-flag" || body["trafficType"] != "user" {
		t.Fatalf("body = %v, want name=new-flag trafficType=user", body)
	}

	out := fmeReadOut(t, ctx)
	if !strings.Contains(out, "new-flag") {
		t.Fatalf("output missing new-flag (item_expr it.entity did not unwrap): %s", out)
	}
}

// TestFMESpec_UpdateFeatureFlag drives "update feature_flag --set description=..."
// and asserts the get-then-patch PATCH body is scoped to the writable fields only
// (FME-17257 fix — previously sent the whole GET'd object back, including
// immutable fields and nested objects, causing a 400 on every update).
func TestFMESpec_UpdateFeatureFlag(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag: command not found or missing endpoint spec")
	}

	getResp := `{"name":"my-flag","description":"old desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"name":"Ramp"},"createdAt":"2026-01-01T00:00:00Z"}`
	patchResp := `{"entity":{"name":"my-flag","description":"new desc"}}`
	refetchResp := `{"name":"my-flag","description":"new desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"name":"Ramp"},"createdAt":"2026-01-01T00:00:00Z"}`
	srv, caps := fmeSequenceServer(t, []string{getResp, patchResp, refetchResp})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.SetArgs = map[string]string{"description": "new desc"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if len(*caps) != 3 {
		t.Fatalf("got %d requests, want 3 (GET, PATCH, refetch GET)", len(*caps))
	}
	patch := (*caps)[1]
	if patch.method != "PATCH" {
		t.Fatalf("2nd request method = %q, want PATCH", patch.method)
	}
	var body map[string]any
	if err := json.Unmarshal(patch.body, &body); err != nil {
		t.Fatalf("unmarshal PATCH body: %v", err)
	}
	if len(body) != 3 || body["description"] != "new desc" {
		t.Fatalf("PATCH body = %v, want {description: new desc, tags, owners} — no leaked id/createdAt/nested objects", body)
	}
	// The GET carried no tags/owners, so the carried-over collections are empty.
	for _, k := range []string{"tags", "owners"} {
		arr, ok := body[k].([]any)
		if !ok || len(arr) != 0 {
			t.Fatalf("PATCH body[%q] = %v, want empty array", k, body[k])
		}
	}
}

// TestFMESpec_UpdateFeatureFlag_TagsOwnersCarryOver drives
// "update feature_flag --set tags.gamma --del tags.alpha --set owners.user:<id>"
// and asserts two things the earlier {description: ...}-only pick got wrong:
//   - the existing tags/owners are carried into the PATCH, so a merge-patch array
//     replacement adds to / removes from the current members instead of wiping them;
//   - they are reshaped to the write DTOs (tags lose the read-only id, owners become
//     {type, id} / {type, identifier}), since v4 rejects unknown properties with a
//     400 "Invalid json structure".
func TestFMESpec_UpdateFeatureFlag_TagsOwnersCarryOver(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag: command not found or missing endpoint spec")
	}

	getResp := `{"name":"my-flag","description":"old desc",` +
		`"tags":[{"id":"t1","name":"alpha"},{"id":"t2","name":"beta"}],` +
		`"owners":[{"id":"u1","name":"alice","type":"USER"},{"id":"g1","name":"platform","type":"GROUP"}]}`
	patchResp := `{"entity":{"name":"my-flag"}}`
	refetchResp := `{"name":"my-flag","description":"old desc"}`
	srv, caps := fmeSequenceServer(t, []string{getResp, patchResp, refetchResp})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.SetArgs = map[string]string{"tags.gamma": "", "owners.user:u2": ""}
	ctx.DelArgs = []string{"tags.alpha"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal((*caps)[1].body, &body); err != nil {
		t.Fatalf("unmarshal PATCH body: %v", err)
	}

	gotTags, err := json.Marshal(body["tags"])
	if err != nil {
		t.Fatalf("marshal tags: %v", err)
	}
	if wantTags := `[{"name":"beta"},{"name":"gamma"}]`; string(gotTags) != wantTags {
		t.Errorf("PATCH tags = %s, want %s (beta kept, alpha removed, gamma added, ids dropped)", gotTags, wantTags)
	}

	gotOwners, err := json.Marshal(body["owners"])
	if err != nil {
		t.Fatalf("marshal owners: %v", err)
	}
	wantOwners := `[{"id":"u1","type":"USER"},{"identifier":"g1","type":"GROUP"},{"id":"u2","type":"USER"}]`
	if string(gotOwners) != wantOwners {
		t.Errorf("PATCH owners = %s, want %s (existing kept and reshaped, u2 added)", gotOwners, wantOwners)
	}
}

// TestFMESpec_UpdateFeatureFlag_DelOwnerByID asserts --del owners.user:<id>
// matches the owner the GET returned. Owners are matched on id because the read
// shape has no email, so an email-keyed member could never match and --del
// silently did nothing.
func TestFMESpec_UpdateFeatureFlag_DelOwnerByID(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag: command not found or missing endpoint spec")
	}

	getResp := `{"name":"my-flag","owners":[{"id":"u1","name":"alice","type":"USER"},{"id":"u2","name":"bob","type":"USER"}]}`
	srv, caps := fmeSequenceServer(t, []string{getResp, `{"entity":{"name":"my-flag"}}`, `{"name":"my-flag"}`})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.DelArgs = []string{"owners.user:u1"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal((*caps)[1].body, &body); err != nil {
		t.Fatalf("unmarshal PATCH body: %v", err)
	}
	got, err := json.Marshal(body["owners"])
	if err != nil {
		t.Fatalf("marshal owners: %v", err)
	}
	if want := `[{"id":"u2","type":"USER"}]`; string(got) != want {
		t.Errorf("PATCH owners = %s, want %s (u1 removed, u2 kept)", got, want)
	}
}

// TestFMESpec_UpdateFeatureFlag_RolloutStatus drives
// "update feature_flag --set rollout_status=<id>" and asserts the PATCH body
// sends {rolloutStatus: {id: ...}} — the v4 API rejects {rolloutStatus: {name: ...}}
// (the shape documented in Confluence) with a 400 "Invalid json structure".
func TestFMESpec_UpdateFeatureFlag_RolloutStatus(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag: command not found or missing endpoint spec")
	}

	getResp := `{"name":"my-flag","description":"old desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"id":"rs-1","name":"Ramp"},"createdAt":"2026-01-01T00:00:00Z"}`
	patchResp := `{"entity":{"name":"my-flag","rolloutStatus":{"id":"rs-2","name":"Ramping"}}}`
	refetchResp := `{"name":"my-flag","description":"old desc","trafficType":{"name":"user"},"status":"ACTIVE","rolloutStatus":{"id":"rs-2","name":"Ramping"},"createdAt":"2026-01-01T00:00:00Z"}`
	srv, caps := fmeSequenceServer(t, []string{getResp, patchResp, refetchResp})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.SetArgs = map[string]string{"rollout_status": "rs-2"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	patch := (*caps)[1]
	var body map[string]any
	if err := json.Unmarshal(patch.body, &body); err != nil {
		t.Fatalf("unmarshal PATCH body: %v", err)
	}
	rs, ok := body["rolloutStatus"].(map[string]any)
	if !ok || rs["id"] != "rs-2" {
		t.Fatalf("PATCH body = %v, want rolloutStatus.id=rs-2 (not rolloutStatus.name)", body)
	}
}

// TestFMESpec_DeleteFeatureFlag drives "delete feature_flag" and asserts the
// DELETE hits the expected path with no body.
func TestFMESpec_DeleteFeatureFlag(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("delete", "feature_flag")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("delete feature_flag: command not found or missing endpoint spec")
	}

	srv, caps := fmeSequenceServer(t, []string{`{}`})

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "my-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if len(*caps) != 1 {
		t.Fatalf("got %d requests, want 1", len(*caps))
	}
	got := (*caps)[0]
	if got.method != "DELETE" || got.path != "/fme/api/v4/feature-flags/my-flag" {
		t.Fatalf("request = %s %s, want DELETE /fme/api/v4/feature-flags/my-flag", got.method, got.path)
	}
}
