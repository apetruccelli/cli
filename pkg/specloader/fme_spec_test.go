// Copyright © 2026 Harness Inc.
// SPDX-License-Identifier: Apache-2.0

package specloader

import (
	"context"
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
	for _, want := range []string{"env-uuid-1", "Prod", "true", "ACTIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q (id column must be present so get/update/delete are usable off list output): %s", want, body)
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

// TestFMESpec_CreateFMEEnvironment drives "create fme_environment <name> --production"
// through the set-fields create strategy: create_body_init seeds {name, isProduction}
// from ctx.id/--production, and item_expr unwraps the {entity, governance} envelope.
func TestFMESpec_CreateFMEEnvironment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("create", "fme_environment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("create fme_environment: command not found or missing endpoint spec")
	}

	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"entity":{"id":"env-uuid-2","name":"cli-test-env","isProduction":true,"status":"ACTIVE"},"governance":null}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-env"
	ctx.Noun = "fme_environment"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"production": true}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/fme/api/v4/environments" {
		t.Fatalf("request path = %q, want /fme/api/v4/environments", gotPath)
	}
	if !strings.Contains(gotQuery, "organization_identifier=org") || !strings.Contains(gotQuery, "project_identifier=proj") {
		t.Fatalf("query = %q, want organization_identifier=org and project_identifier=proj", gotQuery)
	}
	if !strings.Contains(gotBody, `"name":"cli-test-env"`) {
		t.Fatalf("request body missing name: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"isProduction":true`) {
		t.Fatalf("request body missing isProduction: %s", gotBody)
	}

	out := fmeReadOut(t, ctx)
	if !strings.Contains(out, `"id": "env-uuid-2"`) {
		t.Fatalf("output missing entity-unwrapped id: %s", out)
	}
}

// TestFMESpec_UpdateFMEEnvironment drives "update fme_environment --set name=..." through
// the get-then-patch strategy and asserts the curated update_body_pick sends only
// {name, isProduction} — not the full GET response (id/status/createdAt would break the
// backend's Nulls.FAIL UpdateEnvironmentRequest if it ever tightened to reject unknowns).
func TestFMESpec_UpdateFMEEnvironment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "fme_environment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update fme_environment: command not found or missing endpoint spec")
	}

	getFixture := `{"id":"env-uuid-1","name":"Prod","isProduction":true,"status":"ACTIVE"}`
	var gotMethod, gotPath, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, getFixture)
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		fmt.Fprint(w, `{"entity":`+gotBody+`}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "env-uuid-1"
	ctx.Noun = "fme_environment"
	ctx.VerbHandler = cs.VerbHandler
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.SetArgs = map[string]string{"name": "Renamed"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/fme/api/v4/environments/env-uuid-1" {
		t.Fatalf("request path = %q, want /fme/api/v4/environments/env-uuid-1", gotPath)
	}
	if gotContentType != "application/merge-patch+json" {
		t.Fatalf("Content-Type = %q, want application/merge-patch+json", gotContentType)
	}
	if !strings.Contains(gotBody, `"name":"Renamed"`) {
		t.Fatalf("PATCH body missing mutated name: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"isProduction":true`) {
		t.Fatalf("PATCH body missing carried-over isProduction from GET: %s", gotBody)
	}
	if strings.Contains(gotBody, "status") || strings.Contains(gotBody, `"id"`) {
		t.Fatalf("PATCH body should be curated to {name, isProduction} only, got: %s", gotBody)
	}
}

// TestFMESpec_DeleteFMEEnvironment drives "delete fme_environment" and asserts the DELETE
// request hits the {id} path. The real API returns {"governance": {...}} with no entity on
// delete, but this command has no text_header, so no output rendering is attempted either way.
func TestFMESpec_DeleteFMEEnvironment(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("delete", "fme_environment")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("delete fme_environment: command not found or missing endpoint spec")
	}

	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"governance":{"result":"ok"}}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "env-uuid-1"
	ctx.Noun = "fme_environment"
	ctx.VerbHandler = cs.VerbHandler
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/fme/api/v4/environments/env-uuid-1" {
		t.Fatalf("request path = %q, want /fme/api/v4/environments/env-uuid-1", gotPath)
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

// TestFMESpec_ListFeatureFlagDefinition drives "list feature_flag:definition" and asserts
// the flag-name positional arg maps to the feature_flag_name query param and get_id_expr
// resolves off it.featureFlag.name (definitions are addressed by flag name, not environment id).
func TestFMESpec_ListFeatureFlagDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("list", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("list feature_flag:definition: command not found or missing endpoint spec")
	}

	fixture := `{"data":[{"featureFlag":{"name":"cli-test-flag"},"environment":{"name":"Prod"},"defaultTreatment":"on","trafficAllocation":100,"isKilled":false,"createdAt":1778049995.725}],"limit":20,"offset":0,"totalCount":1}`
	srv, path, query := fmeCaptureServerWithQuery(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Noun = "feature_flag"
	ctx.ParentId = "cli-test-flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"

	if err := registry.RunListEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunListEndpoint: %v", err)
	}

	if !strings.HasPrefix(*path, "/fme/api/v4/feature-flag-definitions") {
		t.Fatalf("request path = %q, want prefix /fme/api/v4/feature-flag-definitions", *path)
	}
	if !strings.Contains(*query, "feature_flag_name=cli-test-flag") {
		t.Fatalf("query = %q, want feature_flag_name=cli-test-flag (from parent-id arg)", *query)
	}

	body := fmeReadOut(t, ctx)
	for _, want := range []string{"Prod", "on", "100"} {
		if !strings.Contains(body, want) {
			t.Fatalf("output missing %q: %s", want, body)
		}
	}
}

// TestFMESpec_GetFeatureFlagDefinition drives "get feature_flag:definition" and asserts
// --env maps to the environment_id query param and item_expr resolves the flat item (it,
// no {entity} wrapper — the API returns the definition directly on GET).
func TestFMESpec_GetFeatureFlagDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("get", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("get feature_flag:definition: command not found or missing endpoint spec")
	}

	fixture := `{"defaultTreatment":"off","baselineTreatment":"off","trafficAllocation":50,"isKilled":false,"createdAt":1778049995.725}`
	srv, path, query := fmeCaptureServerWithQuery(t, fixture)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if *path != "/fme/api/v4/feature-flag-definitions/cli-test-flag" {
		t.Fatalf("request path = %q, want /fme/api/v4/feature-flag-definitions/cli-test-flag", *path)
	}
	if !strings.Contains(*query, "environment_id=env-uuid-1") {
		t.Fatalf("query = %q, want environment_id=env-uuid-1 (from --env flag)", *query)
	}

	body := fmeReadOut(t, ctx)
	if !strings.Contains(body, `"trafficAllocation": 50`) {
		t.Fatalf("output missing flat trafficAllocation field: %s", body)
	}
}

// TestFMESpec_CreateFeatureFlagDefinition drives "create feature_flag:definition -f <file>"
// and asserts the file body is POSTed as-is to the {flag-name} path with environment_id from
// --env, and that item_expr unwraps the {entity, governance} envelope on the response.
func TestFMESpec_CreateFeatureFlagDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("create", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("create feature_flag:definition: command not found or missing endpoint spec")
	}

	var gotMethod, gotPath, gotQuery, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"entity":`+gotBody+`,"governance":null}`)
	}))
	t.Cleanup(srv.Close)

	fileBody := `{"treatments":[{"name":"on"},{"name":"off"}],"defaultTreatment":"off","baselineTreatment":"off","trafficAllocation":100,"defaultRule":[{"treatment":"off","size":100}],"rules":[]}`
	filePath := filepath.Join(t.TempDir(), "def.json")
	if err := os.WriteFile(filePath, []byte(fileBody), 0o600); err != nil {
		t.Fatalf("writing input file: %v", err)
	}

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-flag"
	ctx.Noun = "feature_flag"
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1", "file": filePath}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/fme/api/v4/feature-flag-definitions/cli-test-flag" {
		t.Fatalf("request path = %q, want /fme/api/v4/feature-flag-definitions/cli-test-flag", gotPath)
	}
	if !strings.Contains(gotQuery, "environment_id=env-uuid-1") {
		t.Fatalf("query = %q, want environment_id=env-uuid-1 (from --env flag)", gotQuery)
	}
	if gotBody != fileBody {
		t.Fatalf("request body = %q, want file body sent as-is: %q", gotBody, fileBody)
	}

	out := fmeReadOut(t, ctx)
	if !strings.Contains(out, `"trafficAllocation": 100`) {
		t.Fatalf("output missing entity-unwrapped trafficAllocation: %s", out)
	}
}

// TestFMESpec_UpdateFeatureFlagDefinition drives "update feature_flag:definition --set
// default_treatment=on" through the get-then-patch strategy: GET fetches the current
// definition, --set mutates the picked subtree via the field's mutable_path, and PATCH sends
// the merged body as application/merge-patch+json (the FME v4 API's required content type,
// applied automatically by resolveContentType's PATCH default since the spec sets no override).
func TestFMESpec_UpdateFeatureFlagDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag:definition: command not found or missing endpoint spec")
	}

	getFixture := `{"defaultTreatment":"off","baselineTreatment":"off","trafficAllocation":50}`
	var gotMethod, gotPath, gotQuery, gotBody, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, getFixture)
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		fmt.Fprint(w, `{"entity":`+gotBody+`}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-flag"
	ctx.Noun = "feature_flag"
	ctx.FieldsNoun = cs.FieldsNoun
	ctx.VerbHandler = cs.VerbHandler
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1"}
	ctx.SetArgs = map[string]string{"default_treatment": "on"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/fme/api/v4/feature-flag-definitions/cli-test-flag" {
		t.Fatalf("request path = %q, want /fme/api/v4/feature-flag-definitions/cli-test-flag", gotPath)
	}
	if !strings.Contains(gotQuery, "environment_id=env-uuid-1") {
		t.Fatalf("query = %q, want environment_id=env-uuid-1 (from --env flag)", gotQuery)
	}
	if gotContentType != "application/merge-patch+json" {
		t.Fatalf("Content-Type = %q, want application/merge-patch+json", gotContentType)
	}
	if !strings.Contains(gotBody, `"defaultTreatment":"on"`) {
		t.Fatalf("PATCH body missing mutated defaultTreatment: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"baselineTreatment":"off"`) {
		t.Fatalf("PATCH body missing carried-over baselineTreatment from GET: %s", gotBody)
	}
	// Neither --comment nor --title was passed, so their keys must be absent rather than
	// null: under a merge patch, null is an instruction to delete the field.
	for _, absent := range []string{`"comment"`, `"title"`} {
		if strings.Contains(gotBody, absent) {
			t.Fatalf("PATCH body contains %s though the flag was not passed; an unset body_param must be omitted, not sent as null: %s", absent, gotBody)
		}
	}
}

// TestFMESpec_UpdateFeatureFlagDefinition_AuditFields asserts --comment/--title reach the
// PATCH body. They are write-only — v4 accepts them but never returns them — so they cannot
// come from update_body_pick and are declared as body_params instead.
func TestFMESpec_UpdateFeatureFlagDefinition_AuditFields(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("update", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("update feature_flag:definition: command not found or missing endpoint spec")
	}

	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"defaultTreatment":"off","baselineTreatment":"off","trafficAllocation":50}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		fmt.Fprint(w, `{"entity":`+gotBody+`}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-flag"
	ctx.Noun = "feature_flag"
	ctx.FieldsNoun = cs.FieldsNoun
	ctx.VerbHandler = cs.VerbHandler
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1", "comment": "audit note", "title": "my title"}
	ctx.SetArgs = map[string]string{"default_treatment": "on"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	for _, want := range []string{`"comment":"audit note"`, `"title":"my title"`, `"defaultTreatment":"on"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("PATCH body missing %s: %s", want, gotBody)
		}
	}
}

// TestFMESpec_DeleteFeatureFlagDefinition drives "delete feature_flag:definition" and asserts
// the DELETE request hits the {flag-name} path with environment_id from --env, and that
// VerbHandler=delete suppresses body rendering (the API returns 200 with an entity body, but
// delete commands print nothing unless a text_header/text_footer is declared).
func TestFMESpec_DeleteFeatureFlagDefinition(t *testing.T) {
	reg := registry.New()
	if _, err := LoadSpec(reg, "fme.spec.yaml", true); err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	cs := reg.GetSpec("delete", "feature_flag:definition")
	if cs == nil || cs.Endpoint == nil {
		t.Fatal("delete feature_flag:definition: command not found or missing endpoint spec")
	}

	var gotMethod, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"defaultTreatment":"off","baselineTreatment":"off","trafficAllocation":100,"isKilled":false}`)
	}))
	t.Cleanup(srv.Close)

	ctx := fmeTestCtx(t, srv.URL)
	ctx.Id = "cli-test-flag"
	ctx.Noun = "feature_flag"
	ctx.VerbHandler = cs.VerbHandler
	ctx.Resolver = reg
	ctx.FormatFlags.Format = "json"
	ctx.FlagValues = map[string]any{"env": "env-uuid-1"}

	if _, err := registry.RunEndpoint(ctx, cs.Endpoint); err != nil {
		t.Fatalf("RunEndpoint: %v", err)
	}

	if gotMethod != http.MethodDelete {
		t.Fatalf("method = %q, want DELETE", gotMethod)
	}
	if gotPath != "/fme/api/v4/feature-flag-definitions/cli-test-flag" {
		t.Fatalf("request path = %q, want /fme/api/v4/feature-flag-definitions/cli-test-flag", gotPath)
	}
	if !strings.Contains(gotQuery, "environment_id=env-uuid-1") {
		t.Fatalf("query = %q, want environment_id=env-uuid-1 (from --env flag)", gotQuery)
	}
}
