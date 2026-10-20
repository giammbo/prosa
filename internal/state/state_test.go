package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sample() *IntentState {
	return &IntentState{Resources: map[string]*ResourceState{
		"subnet-a": {
			Provider: "hashicorp/aws", Type: "aws_subnet", SchemaVersion: 1,
			State:           json.RawMessage(`{"id":"subnet-0a1","cidr_block":"10.0.1.0/24"}`),
			Private:         []byte{1, 2, 3},
			Identity:        json.RawMessage(`{"id":"subnet-0a1"}`),
			IdentityVersion: 2,
		},
	}}
}

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir, "pass")
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s, _ := open(t)
	st := sample()
	d1, err := s.Save("net", st)
	if err != nil || st.Serial != 1 {
		t.Fatalf("save: %v serial=%d", err, st.Serial)
	}
	got, d, err := s.Load("net")
	if err != nil {
		t.Fatal(err)
	}
	if d != d1 || got.Serial != 1 || !reflect.DeepEqual(got.Resources["subnet-a"].Private, []byte{1, 2, 3}) {
		t.Fatalf("bad load: %+v digest %s vs %s", got, d, d1)
	}
	r := got.Resources["subnet-a"]
	if r.Type != "aws_subnet" || r.IdentityVersion != 2 || string(r.State) != `{"id":"subnet-0a1","cidr_block":"10.0.1.0/24"}` || string(r.Identity) != `{"id":"subnet-0a1"}` {
		t.Fatalf("bad resource: %+v", r)
	}
	if _, err := s.Save("net", got); err != nil || got.Serial != 2 {
		t.Fatalf("second save serial=%d err=%v", got.Serial, err)
	}
	again, _, _ := s.Load("net")
	if again.Serial != 2 {
		t.Fatalf("serial %d", again.Serial)
	}
}

func TestLoadMissingIsEmpty(t *testing.T) {
	s, _ := open(t)
	st, d, err := s.Load("none")
	if err != nil || d != "" || st.Serial != 0 || st.Resources == nil || len(st.Resources) != 0 {
		t.Fatalf("got %+v %q %v", st, d, err)
	}
}

func TestFileIsNotPlaintext(t *testing.T) {
	s, dir := open(t)
	if _, err := s.Save("net", sample()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "net.state"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("subnet-0a1")) || !bytes.HasPrefix(b, []byte("PRST1\n")) {
		t.Fatal("file is plaintext or lacks magic")
	}
}

func TestDigestStableAcrossSaves(t *testing.T) {
	s, dir := open(t)
	st := sample()
	d1, err := s.Save("net", st)
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(filepath.Join(dir, "net.state"))
	st.Serial = 0 // same content at the same Serial, re-encrypted with fresh salt/nonce
	d2, err := s.Save("net", st)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(filepath.Join(dir, "net.state"))
	if bytes.Equal(b1, b2) {
		t.Fatal("expected different ciphertext (fresh salt)")
	}
	if d1 != d2 || !strings.HasPrefix(d1, "sha256:") {
		t.Fatalf("digests differ: %s %s", d1, d2)
	}
	st.Serial = 5
	d3, _ := s.Save("net", st) // Serial is now 6: digest must change
	if d3 == d1 {
		t.Fatal("digest must depend on Serial")
	}
}

func TestWrongPassphraseFails(t *testing.T) {
	s, dir := open(t)
	if _, err := s.Save("net", sample()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "net.state")
	before, _ := os.ReadFile(p)
	bad, err := Open(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bad.Load("net"); err == nil || !strings.Contains(err.Error(), "decrypt") {
		t.Fatalf("want decrypt error, got %v", err)
	}
	after, _ := os.ReadFile(p)
	if !bytes.Equal(before, after) {
		t.Fatal("file changed")
	}
}

func TestTamperedFileFails(t *testing.T) {
	s, dir := open(t)
	if _, err := s.Save("net", sample()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "net.state")
	b, _ := os.ReadFile(p)
	b[len(b)-3] ^= 0xff
	os.WriteFile(p, b, 0o600)
	if _, _, err := s.Load("net"); err == nil {
		t.Fatal("expected error")
	}
}

func TestRenamedFileFails(t *testing.T) {
	s, dir := open(t)
	if _, err := s.Save("a", sample()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.state"))
	os.WriteFile(filepath.Join(dir, "b.state"), b, 0o600)
	if _, _, err := s.Load("b"); err == nil {
		t.Fatal("expected error")
	}
}

func TestLockIsExclusive(t *testing.T) {
	s, dir := open(t)
	unlock, err := s.Lock("net")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("net"); err == nil || !strings.Contains(err.Error(), "is locked") || !strings.Contains(err.Error(), filepath.Join(dir, "net.lock")) {
		t.Fatalf("want a locked error naming the lock file, got %v", err)
	}
	unlock()
	unlock() // idempotent
	u2, err := s.Lock("net")
	if err != nil {
		t.Fatal(err)
	}
	u2()
}

// lockHelperEnv makes TestLockHelperProcess hold the lock of intent "net"
// in the store directory it names.
const lockHelperEnv = "PROSA_STATE_LOCK_HELPER"

// TestLockHelperProcess is not a test: run by TestLockReleasedWhenHolderDies
// in a child process, it takes a lock, says so and waits to be killed.
func TestLockHelperProcess(t *testing.T) {
	dir := os.Getenv(lockHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	s, err := Open(dir, "pass")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock("net"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("locked")
	select {}
}

// A process that dies without unlocking (killed, or os.Exit on a second
// Ctrl-C) does not leave its lock behind (ADR-024).
func TestLockReleasedWhenHolderDies(t *testing.T) {
	s, dir := open(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$")
	cmd.Env = append(os.Environ(), lockHelperEnv+"="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("helper said %q (%v)", line, err)
	}
	if _, err := s.Lock("net"); err == nil || !strings.Contains(err.Error(), "is locked") {
		t.Fatalf("lock held by another process: got %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	unlock, err := s.Lock("net")
	if err != nil {
		t.Fatalf("lock of a dead process not released: %v", err)
	}
	unlock()
}

func TestEmptyPassphraseFails(t *testing.T) {
	if _, err := Open(t.TempDir(), ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestRemoveAndInvalidName(t *testing.T) {
	s, dir := open(t)
	s.Save("net", sample())
	if err := s.Remove("net"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "net.state")); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}
	if err := s.Remove("net"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("../x"); err == nil {
		t.Fatal("expected invalid name error")
	}
}

// A record keeps the lock entry written with it (ADR-024): chosen, with its
// numbers unchanged, depends_on and id. A record without them still loads.
func TestResourceStateKeepsLockEntry(t *testing.T) {
	s, _ := open(t)
	st := sample()
	r := st.Resources["subnet-a"]
	r.ID, r.DependsOn = "subnet-0a1", []string{"net/vpc"}
	r.Chosen = map[string]any{"cidr_block": "10.0.1.0/24", "big": json.Number("9007199254740993"), "tags": map[string]any{"team": "a"}}
	if _, err := s.Save("net", st); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load("net")
	if err != nil {
		t.Fatal(err)
	}
	g := got.Resources["subnet-a"]
	if g.ID != "subnet-0a1" || !reflect.DeepEqual(g.DependsOn, []string{"net/vpc"}) || !reflect.DeepEqual(g.Chosen, r.Chosen) {
		t.Fatalf("record = %+v, want id, depends_on and chosen %v", g, r.Chosen)
	}

	old := sample()
	if _, err := s.Save("old", old); err != nil {
		t.Fatal(err)
	}
	o, _, err := s.Load("old")
	if err != nil {
		t.Fatal(err)
	}
	if rs := o.Resources["subnet-a"]; rs.ID != "" || rs.Chosen != nil || rs.DependsOn != nil {
		t.Fatalf("record without the lock entry = %+v", rs)
	}
}
