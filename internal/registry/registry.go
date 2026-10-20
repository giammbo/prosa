// Package registry downloads provider plugins from a Terraform/OpenTofu
// registry, verifies them (GPG signature of SHA256SUMS, SHA256 of the zip) and
// keeps a verified cache. A binary is never returned unless it was verified.
package registry

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://registry.opentofu.org"
	binaryPrefix   = "terraform-provider-"
	shaFile        = ".sha256"
	metaFile       = ".meta.json"

	maxJSONBytes   = 64 << 20 // version listings and key material
	maxSumsBytes   = 1 << 20
	maxBinaryBytes = 4 << 30 // guards against zip bombs
)

// Provider identifies a provider in the registry, e.g. hashicorp/aws.
type Provider struct {
	Namespace, Name string
}

// Client talks to a registry and owns the local provider cache.
type Client struct {
	BaseURL  string
	HTTP     *http.Client
	CacheDir string
}

// Installed describes a verified provider binary on disk.
type Installed struct {
	Provider  Provider
	Version   string
	OS, Arch  string
	Path      string // the executable
	Protocols []string
	SHA256    string // SHA256 of the downloaded zip, as published by the registry
}

// NewClient returns a client for registry.opentofu.org with the default cache:
// $PROSA_CACHE_DIR if set, otherwise <user cache dir>/prosa/providers. It
// fails when neither is available: a shared temporary directory is not a
// safe place for verified binaries.
func NewClient() (*Client, error) {
	dir := os.Getenv("PROSA_CACHE_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("no provider cache directory: set PROSA_CACHE_DIR (%w)", err)
		}
		dir = filepath.Join(base, "prosa", "providers")
	}
	return &Client{
		BaseURL:  defaultBaseURL,
		HTTP:     &http.Client{Timeout: 10 * time.Minute},
		CacheDir: dir,
	}, nil
}

type versionsResponse struct {
	Versions []struct {
		Version   string `json:"version"`
		Platforms []struct {
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"platforms"`
	} `json:"versions"`
}

// Latest returns the highest stable version with the given major that is
// published for the platform. Prereleases are ignored.
func (c *Client) Latest(ctx context.Context, p Provider, major int, goos, goarch string) (string, error) {
	var resp versionsResponse
	u := fmt.Sprintf("%s/v1/providers/%s/%s/versions", c.base(), p.Namespace, p.Name)
	if err := c.getJSON(ctx, u, &resp); err != nil {
		return "", err
	}
	best, bestSV := "", semver{}
	for _, v := range resp.Versions {
		sv, ok := parseStable(v.Version)
		if !ok || sv[0] != major {
			continue
		}
		onPlatform := false
		for _, pl := range v.Platforms {
			if pl.OS == goos && pl.Arch == goarch {
				onPlatform = true
				break
			}
		}
		if !onPlatform || (best != "" && !sv.greater(bestSV)) {
			continue
		}
		best, bestSV = v.Version, sv
	}
	if best == "" {
		return "", fmt.Errorf("no stable version of %s/%s with major %d for %s_%s", p.Namespace, p.Name, major, goos, goarch)
	}
	return best, nil
}

type downloadInfo struct {
	Protocols           []string `json:"protocols"`
	OS                  string   `json:"os"`
	Arch                string   `json:"arch"`
	Filename            string   `json:"filename"`
	DownloadURL         string   `json:"download_url"`
	ShasumsURL          string   `json:"shasums_url"`
	ShasumsSignatureURL string   `json:"shasums_signature_url"`
	Shasum              string   `json:"shasum"`
	SigningKeys         struct {
		GPGPublicKeys []struct {
			KeyID      string `json:"key_id"`
			ASCIIArmor string `json:"ascii_armor"`
		} `json:"gpg_public_keys"`
	} `json:"signing_keys"`
}

// cacheMeta is the verified metadata persisted next to the cached binary.
type cacheMeta struct {
	SHA256    string   `json:"sha256"` // zip shasum, verified at install time
	Protocols []string `json:"protocols"`
}

// Install returns the verified provider binary. A cache hit (binary hash equal
// to the one recorded at install time) is served without contacting the
// registry; SHA256 and Protocols then come from the metadata persisted when
// the download was verified.
func (c *Client) Install(ctx context.Context, p Provider, version, goos, goarch string) (Installed, error) {
	dir, err := c.cacheDir(p, version, goos, goarch)
	if err != nil {
		return Installed{}, err
	}
	inst := Installed{Provider: p, Version: version, OS: goos, Arch: goarch}
	if path, meta, ok := cachedBinary(dir); ok {
		inst.Path, inst.SHA256, inst.Protocols = path, meta.SHA256, meta.Protocols
		return inst, nil
	}

	u := fmt.Sprintf("%s/v1/providers/%s/%s/%s/download/%s/%s", c.base(), p.Namespace, p.Name, version, goos, goarch)
	var info downloadInfo
	if err := c.getJSON(ctx, u, &info); err != nil {
		return Installed{}, err
	}
	if info.OS != goos || info.Arch != goarch || !strings.HasSuffix(info.Filename, fmt.Sprintf("_%s_%s_%s.zip", version, goos, goarch)) {
		return Installed{}, fmt.Errorf("metadata mismatch: asked for %s %s_%s, registry returned os=%q arch=%q filename=%q",
			version, goos, goarch, info.OS, info.Arch, info.Filename)
	}
	if err := c.download(ctx, info, dir); err != nil {
		return Installed{}, fmt.Errorf("installing %s/%s %s: %w", p.Namespace, p.Name, version, err)
	}
	path, meta, ok := cachedBinary(dir)
	if !ok {
		return Installed{}, errors.New("installed binary failed post-install verification")
	}
	inst.Path, inst.SHA256, inst.Protocols = path, meta.SHA256, meta.Protocols
	return inst, nil
}

// download verifies and extracts the provider into finalDir. Everything is
// staged in a sibling temp directory and renamed at the end, so an interrupted
// install never leaves a directory that looks valid.
func (c *Client) download(ctx context.Context, info downloadInfo, finalDir string) error {
	var keys []string
	for _, k := range info.SigningKeys.GPGPublicKeys {
		keys = append(keys, k.ASCIIArmor)
	}
	sums, err := c.getBytes(ctx, info.ShasumsURL, maxSumsBytes)
	if err != nil {
		return err
	}
	sig, err := c.getBytes(ctx, info.ShasumsSignatureURL, maxSumsBytes)
	if err != nil {
		return err
	}
	if err := verifySignature(sums, sig, keys); err != nil {
		return err
	}
	listed, err := shasumFor(sums, info.Filename)
	if err != nil {
		return err
	}
	want := strings.ToLower(info.Shasum)
	if listed != want {
		return fmt.Errorf("sha256 mismatch: SHA256SUMS lists %s for %s but the registry advertises %s", listed, info.Filename, want)
	}

	parent := filepath.Dir(finalDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".install-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	zipPath := filepath.Join(stage, "download.zip")
	if err := c.fetchToFile(ctx, info.DownloadURL, zipPath); err != nil {
		return err
	}
	got, err := hashFile(zipPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sha256 mismatch: downloaded zip is %s, expected %s", got, want)
	}

	outDir := filepath.Join(stage, "out")
	if err := os.Mkdir(outDir, 0o755); err != nil {
		return err
	}
	exe, err := extractProvider(zipPath, outDir)
	if err != nil {
		return err
	}
	exeSum, err := hashFile(filepath.Join(outDir, exe))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, shaFile), []byte(exeSum+"\n"), 0o644); err != nil {
		return err
	}
	mb, err := json.Marshal(cacheMeta{SHA256: want, Protocols: info.Protocols})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, metaFile), mb, 0o644); err != nil {
		return err
	}

	if err := os.RemoveAll(finalDir); err != nil { // replaces a corrupt cache entry
		return err
	}
	return os.Rename(outDir, finalDir)
}

// extractProvider writes the single terraform-provider-* entry of the zip to
// dir (executable) and returns its file name.
func extractProvider(zipPath, dir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("opening provider zip: %w", err)
	}
	defer zr.Close()
	var entry *zip.File
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !strings.HasPrefix(filepath.Base(f.Name), binaryPrefix) {
			continue
		}
		if entry != nil {
			return "", errors.New("provider zip contains more than one terraform-provider-* entry")
		}
		entry = f
	}
	if entry == nil {
		return "", errors.New("provider zip contains no terraform-provider-* entry")
	}
	name := filepath.Base(entry.Name) // never trust paths inside the archive
	rc, err := entry.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(out, io.LimitReader(rc, maxBinaryBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxBinaryBytes {
		return "", errors.New("provider binary exceeds size limit")
	}
	return name, nil
}

// cachedBinary returns the executable in dir and its persisted metadata only
// if the binary's hash matches the recorded one. Any anomaly means "not cached".
func cachedBinary(dir string) (string, cacheMeta, bool) {
	var meta cacheMeta
	recorded, err := os.ReadFile(filepath.Join(dir, shaFile))
	if err != nil {
		return "", meta, false
	}
	mb, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil || json.Unmarshal(mb, &meta) != nil || meta.SHA256 == "" {
		return "", meta, false
	}
	// os.ReadDir, not filepath.Glob: the cache path may contain glob
	// metacharacters such as '['.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", meta, false
	}
	var matches []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), binaryPrefix) {
			matches = append(matches, filepath.Join(dir, e.Name()))
		}
	}
	if len(matches) != 1 {
		return "", meta, false
	}
	st, err := os.Stat(matches[0])
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0o111 == 0 {
		return "", meta, false
	}
	sum, err := hashFile(matches[0])
	if err != nil || sum != strings.TrimSpace(string(recorded)) {
		return "", meta, false
	}
	return matches[0], meta, true
}

func (c *Client) cacheDir(p Provider, version, goos, goarch string) (string, error) {
	u, err := url.Parse(c.base())
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid registry URL %q", c.base())
	}
	host := strings.NewReplacer(":", "_", "/", "_", `\`, "_").Replace(u.Host)
	for _, seg := range []string{p.Namespace, p.Name, version, goos, goarch} {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, `/\`) {
			return "", fmt.Errorf("invalid path segment %q", seg)
		}
	}
	return filepath.Join(c.CacheDir, host, p.Namespace, p.Name, version, goos+"_"+goarch), nil
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp, nil
}

func (c *Client) getBytes(ctx context.Context, u string, limit int64) ([]byte, error) {
	resp, err := c.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", u, limit)
	}
	return b, nil
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	b, err := c.getBytes(ctx, u, maxJSONBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", u, err)
	}
	return nil
}

func (c *Client) fetchToFile(ctx context.Context, u, path string) error {
	resp, err := c.get(ctx, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

type semver [3]int

func (a semver) greater(b semver) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// parseStable parses MAJOR.MINOR.PATCH, rejecting prereleases and build metadata.
func parseStable(v string) (semver, bool) {
	parts := strings.Split(v, ".")
	var sv semver
	if len(parts) != 3 {
		return sv, false
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return sv, false
		}
		sv[i] = n
	}
	return sv, true
}
