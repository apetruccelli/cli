// Copyright © 2026 Harness Inc.
// SPDX-License-Identifier: Apache-2.0

package har

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/harness/cli/v3/pkg/cmdctx"
)

const (
	terraformTarGzExt      = ".tar.gz"
	terraformTgzExt        = ".tgz"
	terraformZipExt        = ".zip"
	terraformMaxModuleSize = 500 * 1024 * 1024 // 500MB
)

// terraformDirSkipNames are file/dir basenames excluded when packaging a
// module directory into a .tar.gz archive.
var terraformDirSkipNames = map[string]bool{
	".git":       true,
	".terraform": true,
	".DS_Store":  true,
}

// terraformProviderFilenameRegex matches terraform-provider-{type}_{version}_{os}_{arch}.zip
// per the Provider Network Mirror Protocol naming convention.
var terraformProviderFilenameRegex = regexp.MustCompile(
	`^terraform-provider-([a-zA-Z0-9-]+)_(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)_([a-z0-9]+)_([a-z0-9]+)\.zip$`,
)

// pushTerraformArtifact implements "push artifact:terraform".
//
// The input path is either a module source directory (packaged into a
// .tar.gz here) or a pre-built file: a module archive (.tar.gz/.tgz) or a
// provider archive (.zip) already named per the Provider Network Mirror
// Protocol convention.
func pushTerraformArtifact(ctx *cmdctx.Ctx) error {
	if len(ctx.Args) == 0 {
		return fmt.Errorf("push terraform artifact requires a file or directory path: push artifact <registry> <path>")
	}

	registry := ctx.Id

	namespace := cmdctx.GetString(ctx.FlagValues, "namespace")
	name := cmdctx.GetString(ctx.FlagValues, "name")
	provider := cmdctx.GetString(ctx.FlagValues, "provider")
	version := cmdctx.GetString(ctx.FlagValues, "version")
	if namespace == "" {
		return fmt.Errorf("--namespace is required")
	}

	inputPath := ctx.Args[0]
	pathInfo, err := os.Stat(inputPath)
	if err != nil {
		return fmt.Errorf("failed to access package path: %w", err)
	}

	if pathInfo.IsDir() {
		archivePath, err := packageModuleDir(inputPath, namespace, name, provider, version)
		if err != nil {
			return err
		}
		defer os.RemoveAll(filepath.Dir(archivePath))
		return pushTerraformModule(ctx, registry, archivePath, namespace, name, provider, version)
	}

	switch {
	case isTerraformModule(inputPath):
		return pushTerraformModule(ctx, registry, inputPath, namespace, name, provider, version)
	case isTerraformProvider(inputPath):
		return pushTerraformProvider(ctx, registry, inputPath, namespace)
	default:
		return fmt.Errorf("unsupported terraform artifact %q: expected a module directory, %s/%s module archive, or %s provider archive",
			inputPath, terraformTarGzExt, terraformTgzExt, terraformZipExt)
	}
}

// packageModuleDir validates a module source directory and packages it into a
// .tar.gz archive named "{ns}-{name}-{provider}-{ver}.tar.gz" in a temp dir.
// The caller owns removing the returned path's parent directory.
func packageModuleDir(dir, namespace, name, provider, version string) (string, error) {
	dir = filepath.Clean(dir)
	if name == "" {
		return "", fmt.Errorf("--name is required to package a module directory")
	}
	if provider == "" {
		return "", fmt.Errorf("--provider is required to package a module directory")
	}
	if version == "" {
		return "", fmt.Errorf("--version is required to package a module directory")
	}
	if _, err := semver.NewVersion(version); err != nil {
		return "", fmt.Errorf("invalid version %q, must be SemVer 2.0.0: %w", version, err)
	}

	hasTf := false
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if terraformDirSkipNames[info.Name()] || strings.Contains(info.Name(), ".tfstate") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".tf") && filepath.Dir(path) == dir {
			hasTf = true
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("failed to scan module directory: %w", err)
	}
	if !hasTf {
		return "", fmt.Errorf("module directory %q must contain at least one .tf file at the root level", dir)
	}

	tmpDir, err := os.MkdirTemp("", "harness-terraform-module-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory for packaging: %w", err)
	}

	archivePath := filepath.Join(tmpDir, fmt.Sprintf("%s-%s-%s-%s%s", namespace, name, provider, version, terraformTarGzExt))
	if err := writeModuleArchive(archivePath, dir); err != nil {
		os.RemoveAll(tmpDir)
		return "", err
	}

	info, err := os.Stat(archivePath)
	if err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("failed to access packaged module archive: %w", err)
	}
	if info.Size() > terraformMaxModuleSize {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("packaged module archive is %d bytes, exceeds max size of %d bytes", info.Size(), terraformMaxModuleSize)
	}

	return archivePath, nil
}

// writeModuleArchive walks dir and writes its contents (skipping VCS/state
// dirs) into a gzip-compressed tar at archivePath.
func writeModuleArchive(archivePath, dir string) error {
	out, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("failed to create archive file: %w", err)
	}
	defer out.Close()

	gzWriter := gzip.NewWriter(out)
	tarWriter := tar.NewWriter(gzWriter)

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if terraformDirSkipNames[info.Name()] || strings.Contains(info.Name(), ".tfstate") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("failed to compute relative path for %s: %w", path, err)
		}

		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("failed to open %s: %w", path, err)
		}
		defer file.Close()

		if err := tarWriter.WriteHeader(&tar.Header{
			Name: filepath.ToSlash(relPath),
			Mode: 0o644,
			Size: info.Size(),
		}); err != nil {
			return fmt.Errorf("failed to write tar header for %s: %w", relPath, err)
		}
		if _, err := io.Copy(tarWriter, file); err != nil {
			return fmt.Errorf("failed to write %s to archive: %w", relPath, err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to build module archive: %w", err)
	}

	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize tar: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return fmt.Errorf("failed to finalize gzip: %w", err)
	}
	return nil
}

// isTerraformModule reports whether path is a module archive (.tar.gz or .tgz).
func isTerraformModule(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, terraformTarGzExt) || strings.HasSuffix(lower, terraformTgzExt)
}

// isTerraformProvider reports whether path is a provider archive (.zip).
func isTerraformProvider(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), terraformZipExt)
}

// pushTerraformModule uploads a pre-built module archive via
// PUT /pkg/{account}/{registry}/terraform/v1/modules/{ns}/{name}/{provider}/{ver}.
func pushTerraformModule(ctx *cmdctx.Ctx, registry, filePath, namespace, name, provider, version string) error {
	if name == "" {
		return fmt.Errorf("--name is required for module uploads")
	}
	if provider == "" {
		return fmt.Errorf("--provider is required for module uploads")
	}
	if version == "" {
		return fmt.Errorf("--version is required for module uploads")
	}
	if _, err := semver.NewVersion(version); err != nil {
		return fmt.Errorf("invalid version %q, must be SemVer 2.0.0: %w", version, err)
	}

	checksums, err := computeFileChecksums(filePath)
	if err != nil {
		return fmt.Errorf("computing checksums: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open package file: %w", err)
	}
	defer file.Close()

	subpath := fmt.Sprintf("%s/terraform/v1/modules/%s/%s/%s/%s", registry, namespace, name, provider, version)
	uploadURL, err := buildPkgURL(ctx.Auth.RegistryURL, ctx.Auth.AccountID, subpath)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Uploading %s ...\n", filepath.Base(filePath))
	req, err := http.NewRequest("PUT", uploadURL, file)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	setAuthHeader(req, ctx.Auth)
	req.Header.Set("Content-Type", "application/octet-stream")
	setChecksumHeaders(req.Header, checksums)

	if _, err := doRequest(newHTTPClient(), req); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Successfully pushed Terraform module %s/%s/%s@%s to registry %q\n", namespace, name, provider, version, registry)
	return nil
}

// pushTerraformProvider uploads a provider binary as-is via
// PUT /pkg/{account}/{registry}/terraform/v1/providers/{ns}/{type}/{ver}/{filename}.
// type/version/os/arch are parsed from the filename, which must already
// follow the terraform-provider-{type}_{version}_{os}_{arch}.zip convention.
func pushTerraformProvider(ctx *cmdctx.Ctx, registry, filePath, namespace string) error {
	filename := filepath.Base(filePath)
	typeName, version, osName, arch, err := parseProviderFilename(filename)
	if err != nil {
		return err
	}
	if _, err := semver.NewVersion(version); err != nil {
		return fmt.Errorf("invalid version %q in filename, must be SemVer 2.0.0: %w", version, err)
	}

	checksums, err := computeFileChecksums(filePath)
	if err != nil {
		return fmt.Errorf("computing checksums: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open package file: %w", err)
	}
	defer file.Close()

	subpath := fmt.Sprintf("%s/terraform/v1/providers/%s/%s/%s/%s", registry, namespace, typeName, version, filename)
	uploadURL, err := buildPkgURL(ctx.Auth.RegistryURL, ctx.Auth.AccountID, subpath)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Uploading %s ...\n", filename)
	req, err := http.NewRequest("PUT", uploadURL, file)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	setAuthHeader(req, ctx.Auth)
	req.Header.Set("Content-Type", "application/octet-stream")
	setChecksumHeaders(req.Header, checksums)

	if _, err := doRequest(newHTTPClient(), req); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Successfully pushed Terraform provider %s/%s@%s (%s_%s) to registry %q\n", namespace, typeName, version, osName, arch, registry)
	return nil
}

// parseProviderFilename extracts type, version, os and arch from a provider
// filename following the terraform-provider-{type}_{version}_{os}_{arch}.zip
// convention mandated by the Provider Network Mirror Protocol.
func parseProviderFilename(filename string) (typeName, version, osName, arch string, err error) {
	m := terraformProviderFilenameRegex.FindStringSubmatch(filename)
	if m == nil {
		return "", "", "", "", fmt.Errorf(
			"filename %q does not match required convention terraform-provider-{type}_{version}_{os}_{arch}.zip",
			filename,
		)
	}
	return m[1], m[2], m[3], m[4], nil
}
