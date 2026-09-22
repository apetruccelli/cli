// Copyright © 2026 Harness Inc.
// SPDX-License-Identifier: Apache-2.0

package har

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/harness/cli/v3/pkg/auth"
	"github.com/harness/cli/v3/pkg/cmdctx"
)

func TestParseProviderFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		wantErr  bool
		wantType string
		wantVer  string
		wantOS   string
		wantArch string
	}{
		{
			name:     "valid",
			filename: "terraform-provider-aws_5.31.0_linux_amd64.zip",
			wantType: "aws",
			wantVer:  "5.31.0",
			wantOS:   "linux",
			wantArch: "amd64",
		},
		{
			name:     "prerelease and build metadata",
			filename: "terraform-provider-my-cloud_1.0.0-beta.1+build.5_darwin_arm64.zip",
			wantType: "my-cloud",
			wantVer:  "1.0.0-beta.1+build.5",
			wantOS:   "darwin",
			wantArch: "arm64",
		},
		{
			name:     "invalid convention",
			filename: "aws-provider-5.31.0.zip",
			wantErr:  true,
		},
		{
			name:     "wrong extension",
			filename: "terraform-provider-aws_5.31.0_linux_amd64.tar.gz",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			typeName, version, osName, arch, err := parseProviderFilename(tt.filename)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.filename)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if typeName != tt.wantType || version != tt.wantVer || osName != tt.wantOS || arch != tt.wantArch {
				t.Errorf("got (%s, %s, %s, %s), want (%s, %s, %s, %s)",
					typeName, version, osName, arch, tt.wantType, tt.wantVer, tt.wantOS, tt.wantArch)
			}
		})
	}
}

func TestIsTerraformModuleAndProvider(t *testing.T) {
	if !isTerraformModule("module.tar.gz") || !isTerraformModule("MODULE.TGZ") {
		t.Error("expected .tar.gz/.tgz to be recognized as modules")
	}
	if isTerraformModule("provider.zip") {
		t.Error("did not expect .zip to be recognized as a module")
	}
	if !isTerraformProvider("terraform-provider-aws_5.31.0_linux_amd64.zip") {
		t.Error("expected .zip to be recognized as a provider")
	}
	if isTerraformProvider("module.tar.gz") {
		t.Error("did not expect .tar.gz to be recognized as a provider")
	}
}

func TestPackageModuleDir(t *testing.T) {
	t.Run("missing root .tf file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := packageModuleDir(dir, "ns", "name", "aws", "1.0.0"); err == nil {
			t.Fatal("expected error for module dir with no root .tf file")
		}
	})

	t.Run("valid module directory", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte("# terraform"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("skip me"), 0o644); err != nil {
			t.Fatal(err)
		}

		archivePath, err := packageModuleDir(dir, "ns", "name", "aws", "1.0.0")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer os.RemoveAll(filepath.Dir(archivePath))

		if filepath.Base(archivePath) != "ns-name-aws-1.0.0.tar.gz" {
			t.Errorf("unexpected archive name: %s", filepath.Base(archivePath))
		}
		if _, err := os.Stat(archivePath); err != nil {
			t.Fatalf("archive not created: %v", err)
		}
	})

	t.Run("missing required fields", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := packageModuleDir(dir, "ns", "", "aws", "1.0.0"); err == nil {
			t.Error("expected error when --name is empty")
		}
		if _, err := packageModuleDir(dir, "ns", "name", "", "1.0.0"); err == nil {
			t.Error("expected error when --provider is empty")
		}
		if _, err := packageModuleDir(dir, "ns", "name", "aws", ""); err == nil {
			t.Error("expected error when --version is empty")
		}
		if _, err := packageModuleDir(dir, "ns", "name", "aws", "not-semver"); err == nil {
			t.Error("expected error for invalid semver version")
		}
	})
}

func terraformTestCtx(flags map[string]any, args []string, registryURL string) *cmdctx.Ctx {
	return &cmdctx.Ctx{
		Context:    context.Background(),
		Id:         "my-registry",
		Args:       args,
		FlagValues: flags,
		Auth: &auth.ResolvedAuth{
			AuthType:    auth.AuthTypePAT,
			RegistryURL: registryURL,
			AccountID:   "acct",
			PATToken:    "test-token",
		},
	}
}

func TestPushTerraformModule_RequestURL(t *testing.T) {
	var gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "module.tar.gz")
	if err := os.WriteFile(filePath, []byte("fake archive"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := terraformTestCtx(nil, nil, srv.URL)
	if err := pushTerraformModule(ctx, "my-registry", filePath, "my-ns", "my-mod", "aws", "1.2.3"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/pkg/acct/my-registry/terraform/v1/modules/my-ns/my-mod/aws/1.2.3"
	if gotPath != wantPath {
		t.Errorf("got path %q, want %q", gotPath, wantPath)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("got content-type %q, want application/octet-stream", gotContentType)
	}
}

func TestPushTerraformProvider_RequestURL(t *testing.T) {
	var gotPath, gotContentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	filename := "terraform-provider-aws_5.31.0_linux_amd64.zip"
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte("fake provider"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := terraformTestCtx(nil, nil, srv.URL)
	if err := pushTerraformProvider(ctx, "my-registry", filePath, "my-ns"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPath := "/pkg/acct/my-registry/terraform/v1/providers/my-ns/aws/5.31.0/" + filename
	if gotPath != wantPath {
		t.Errorf("got path %q, want %q", gotPath, wantPath)
	}
	if gotContentType != "application/octet-stream" {
		t.Errorf("got content-type %q, want application/octet-stream", gotContentType)
	}
}
