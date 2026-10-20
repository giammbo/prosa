package registry

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

const (
	testVersion  = "6.69.1"
	testFilename = "terraform-provider-aws_6.69.1_darwin_arm64.zip"
	testExeName  = "terraform-provider-aws_v6.69.1_x5"
	testExeBody  = "#!/bin/sh\necho fake provider\n"
)

var awsProvider = Provider{Namespace: "hashicorp", Name: "aws"}

type fakeRegistry struct {
	srv        *httptest.Server
	signer     *openpgp.Entity  // key that signs SHA256SUMS
	publishKey *openpgp.Entity  // key advertised in the JSON
	zipBytes   []byte           // zip advertised (shasum, SHA256SUMS)
	served     []byte           // if set, served instead of zipBytes
	zipHandler http.HandlerFunc // if set, serves the zip instead
	zipHits    atomic.Int32
	armoredSig bool
	metaHits   atomic.Int32
	// overrides for the metadata JSON
	metaShasum, metaOS, metaArch, metaProtocols string
}

func newEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity("test", "", "test@example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func buildZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func armoredPublicKey(t *testing.T, e *openpgp.Entity) string {
	t.Helper()
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{signer: newEntity(t)}
	f.publishKey = f.signer
	f.zipBytes = buildZip(t, map[string]string{
		testExeName: testExeBody,
		"LICENSE":   "mpl",
	})
	genuine := func() string { s := sha256.Sum256(f.zipBytes); return hex.EncodeToString(s[:]) }
	sums := func() string { return fmt.Sprintf("%s  %s\n", genuine(), testFilename) }

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/providers/hashicorp/aws/versions", func(w http.ResponseWriter, r *http.Request) {
		plat := `"platforms":[{"os":"darwin","arch":"arm64"},{"os":"linux","arch":"amd64"}]`
		fmt.Fprintf(w, `{"versions":[
			{"version":"6.68.0","protocols":["5.0"],%[1]s},
			{"version":"6.69.1","protocols":["5.0"],%[1]s},
			{"version":"7.0.0-beta1","protocols":["5.0"],%[1]s},
			{"version":"5.99.0","protocols":["5.0"],%[1]s},
			{"version":"6.70.0","protocols":["5.0"],"platforms":[{"os":"linux","arch":"amd64"}]}
		]}`, plat)
	})
	mux.HandleFunc("/v1/providers/hashicorp/aws/6.69.1/download/darwin/arm64", func(w http.ResponseWriter, r *http.Request) {
		f.metaHits.Add(1)
		sum, osn, arch, protos := genuine(), "darwin", "arm64", `["5.0"]`
		if f.metaShasum != "" {
			sum = f.metaShasum
		}
		if f.metaOS != "" {
			osn = f.metaOS
		}
		if f.metaArch != "" {
			arch = f.metaArch
		}
		if f.metaProtocols != "" {
			protos = f.metaProtocols
		}
		fmt.Fprintf(w, `{"protocols":%s,"os":%q,"arch":%q,"filename":%q,
			"download_url":%q,"shasums_url":%q,"shasums_signature_url":%q,"shasum":%q,
			"signing_keys":{"gpg_public_keys":[{"key_id":"ABCD","ascii_armor":%q}]}}`,
			protos, osn, arch,
			testFilename, f.srv.URL+"/dl/"+testFilename, f.srv.URL+"/dl/SHA256SUMS",
			f.srv.URL+"/dl/SHA256SUMS.sig", sum, armoredPublicKey(t, f.publishKey))
	})
	mux.HandleFunc("/dl/"+testFilename, func(w http.ResponseWriter, r *http.Request) {
		f.zipHits.Add(1)
		if f.zipHandler != nil {
			f.zipHandler(w, r)
			return
		}
		if f.served != nil {
			w.Write(f.served)
			return
		}
		w.Write(f.zipBytes)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sums()))
	})
	mux.HandleFunc("/dl/SHA256SUMS.sig", func(w http.ResponseWriter, r *http.Request) {
		var sig bytes.Buffer
		var err error
		if f.armoredSig {
			err = openpgp.ArmoredDetachSign(&sig, f.signer, strings.NewReader(sums()), nil)
		} else {
			err = openpgp.DetachSign(&sig, f.signer, strings.NewReader(sums()), nil)
		}
		if err != nil {
			t.Error(err)
		}
		w.Write(sig.Bytes())
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) client(t *testing.T) *Client {
	return &Client{BaseURL: f.srv.URL, HTTP: f.srv.Client(), CacheDir: t.TempDir()}
}

func TestLatestPicksHighestStableForMajor(t *testing.T) {
	f := newFakeRegistry(t)
	got, err := f.client(t).Latest(context.Background(), awsProvider, 6, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "6.69.1" {
		t.Fatalf("Latest = %q, want 6.69.1", got)
	}
	if _, err := f.client(t).Latest(context.Background(), awsProvider, 9, "darwin", "arm64"); err == nil {
		t.Fatal("expected error for a major with no versions")
	}
}

func TestInstallVerifiesAndExtracts(t *testing.T) {
	for _, armored := range []bool{false, true} {
		t.Run(fmt.Sprintf("armoredSig=%v", armored), func(t *testing.T) {
			f := newFakeRegistry(t)
			f.armoredSig = armored
			inst, err := f.client(t).Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
			if err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(inst.Path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode()&0o111 == 0 {
				t.Fatalf("binary not executable: %v", st.Mode())
			}
			body, _ := os.ReadFile(inst.Path)
			if string(body) != testExeBody {
				t.Fatalf("unexpected binary content %q", body)
			}
			want := sha256.Sum256(f.zipBytes)
			if inst.SHA256 != hex.EncodeToString(want[:]) {
				t.Fatalf("SHA256 = %s", inst.SHA256)
			}
			if inst.Version != testVersion || inst.OS != "darwin" || inst.Arch != "arm64" ||
				inst.Provider != awsProvider || len(inst.Protocols) != 1 || inst.Protocols[0] != "5.0" {
				t.Fatalf("unexpected Installed: %+v", inst)
			}
		})
	}
}

func TestInstallRejectsShaMismatch(t *testing.T) {
	f := newFakeRegistry(t)
	f.served = buildZip(t, map[string]string{testExeName: "tampered"})
	c := f.client(t)
	_, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256 mismatch, got %v", err)
	}
	_ = filepath.WalkDir(c.CacheDir, func(p string, _ fs.DirEntry, _ error) error {
		if strings.Contains(p, "terraform-provider-") {
			t.Errorf("a rejected download must not leave a binary in the cache: %s", p)
		}
		return nil
	})
}

func TestInstallRejectsBadSignature(t *testing.T) {
	f := newFakeRegistry(t)
	f.signer = newEntity(t) // signs with a key that is not the advertised one
	_, err := f.client(t).Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("want signature error, got %v", err)
	}
}

func TestInstallUsesCache(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client(t)
	first, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if f.zipHits.Load() != 1 {
		t.Fatalf("zip hits after first install = %d", f.zipHits.Load())
	}
	second, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if f.zipHits.Load() != 1 {
		t.Fatalf("second install downloaded the zip again (%d hits)", f.zipHits.Load())
	}
	if first.Path != second.Path || second.SHA256 != first.SHA256 {
		t.Fatalf("cache hit returned different result: %+v vs %+v", first, second)
	}
}

func TestInstallReinstallsCorruptCache(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client(t)
	inst, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inst.Path, []byte("#!/bin"), 0o755); err != nil { // truncated
		t.Fatal(err)
	}
	again, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if f.zipHits.Load() != 2 {
		t.Fatalf("corrupt cache must trigger a re-download, zip hits = %d", f.zipHits.Load())
	}
	body, _ := os.ReadFile(again.Path)
	if string(body) != testExeBody {
		t.Fatalf("binary not restored, got %q", body)
	}
}

func TestInstallRejectsZipWithoutSingleProviderBinary(t *testing.T) {
	f := newFakeRegistry(t)
	f.zipBytes = buildZip(t, map[string]string{"README": "x"})
	_, err := f.client(t).Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "no terraform-provider-* entry") {
		t.Fatalf("want missing-entry error, got %v", err)
	}
}

func TestInstallCacheHitReturnsVerifiedMetadataWithoutRegistry(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client(t)
	first, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	f.metaShasum = strings.Repeat("ab", 32)
	f.metaProtocols = `["6.0"]`
	metaBefore, zipBefore := f.metaHits.Load(), f.zipHits.Load()
	second, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA256 != first.SHA256 || len(second.Protocols) != 1 || second.Protocols[0] != "5.0" {
		t.Fatalf("cache hit returned unverified metadata: %+v", second)
	}
	if f.metaHits.Load() != metaBefore || f.zipHits.Load() != zipBefore {
		t.Fatal("cache hit must not contact the registry")
	}
}

func TestInstallRejectsMetadataMismatch(t *testing.T) {
	f := newFakeRegistry(t)
	f.metaOS, f.metaArch = "linux", "amd64"
	c := f.client(t)
	_, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "metadata mismatch") {
		t.Fatalf("want metadata mismatch, got %v", err)
	}
	if f.zipHits.Load() != 0 {
		t.Fatal("zip must not be downloaded on metadata mismatch")
	}
	_ = filepath.WalkDir(c.CacheDir, func(p string, _ fs.DirEntry, _ error) error {
		if strings.Contains(p, "terraform-provider-") {
			t.Errorf("nothing must be installed: %s", p)
		}
		return nil
	})
}

func TestNewClientWithoutCacheDirFails(t *testing.T) {
	t.Setenv("PROSA_CACHE_DIR", "")
	t.Setenv("HOME", "")           // os.UserCacheDir on darwin and linux
	t.Setenv("XDG_CACHE_HOME", "") // linux
	if c, err := NewClient(); err == nil {
		t.Fatalf("want an error without a cache dir, got client with CacheDir %q", c.CacheDir)
	}

	t.Setenv("PROSA_CACHE_DIR", "/somewhere/prosa")
	c, err := NewClient()
	if err != nil || c.CacheDir != "/somewhere/prosa" {
		t.Fatalf("PROSA_CACHE_DIR: got (%+v, %v)", c, err)
	}
}

func TestInstallCacheDirWithGlobMetacharacters(t *testing.T) {
	f := newFakeRegistry(t)
	c := f.client(t)
	c.CacheDir = filepath.Join(t.TempDir(), "cache[1]*?")
	first, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Install(context.Background(), awsProvider, testVersion, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if f.zipHits.Load() != 1 {
		t.Fatalf("second install downloaded again (%d zip hits): cache lookup broken by the path", f.zipHits.Load())
	}
	if first.Path != second.Path {
		t.Fatalf("paths differ: %s vs %s", first.Path, second.Path)
	}
}

func TestInstallCancelledMidDownloadRemovesStaging(t *testing.T) {
	f := newFakeRegistry(t)
	started := make(chan struct{})
	f.zipHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Write(f.zipBytes[:len(f.zipBytes)/2])
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done() // hang until the client goes away
	}
	c := f.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	done := make(chan error, 1)
	go func() {
		_, err := c.Install(ctx, awsProvider, testVersion, "darwin", "arm64")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Install did not return after cancellation")
	}
	_ = filepath.WalkDir(c.CacheDir, func(p string, d fs.DirEntry, _ error) error {
		if d != nil && strings.HasPrefix(d.Name(), ".install-") {
			t.Errorf("staging directory left behind: %s", p)
		}
		return nil
	})
}
