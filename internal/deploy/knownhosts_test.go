package deploy

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real ed25519 key generated with ssh-keygen; the expected fingerprint is the
// exact `ssh-keygen -lf` output for it.
const (
	fixtureEd25519Key = "AAAAC3NzaC1lZDI1NTE5AAAAIOC1fBGmprTRgKWy2+g5QqqOQ6X95acZ6pbByKW5b8yK"
	fixtureEd25519FP  = "SHA256:f/+IMT34E8qsxk2X/ZKlziiV1CdRTKI1BGin66IkrsQ"
)

func TestFingerprintMatchesSSHKeygen(t *testing.T) {
	got, err := Fingerprint(fixtureEd25519Key)
	if err != nil {
		t.Fatalf("Fingerprint() error = %v", err)
	}
	if got != fixtureEd25519FP {
		t.Fatalf("Fingerprint() = %q, want %q", got, fixtureEd25519FP)
	}
	if strings.HasSuffix(got, "=") {
		t.Fatalf("Fingerprint() = %q, must be unpadded base64", got)
	}
}

func TestFingerprintRejectsBadBase64(t *testing.T) {
	if _, err := Fingerprint("not base64 !!!"); !errors.Is(err, ErrTrust) {
		t.Fatalf("Fingerprint() error = %v, want ErrTrust", err)
	}
}

func TestParseKeyscanOutputSkipsCommentsAndBlanks(t *testing.T) {
	out := "# example.com:22 SSH-2.0-OpenSSH_9.6\n" +
		"example.com ssh-rsa AAAAB3rsa\n" +
		"\n" +
		"example.com ssh-ed25519 " + fixtureEd25519Key + "\n" +
		"short line\n"
	keys := ParseKeyscanOutput(out)
	if len(keys) != 2 {
		t.Fatalf("ParseKeyscanOutput() = %+v, want 2 keys", keys)
	}
	if keys[0].Type != "ssh-rsa" || keys[1].Type != "ssh-ed25519" {
		t.Fatalf("ParseKeyscanOutput() types = %q,%q", keys[0].Type, keys[1].Type)
	}
	if keys[1].Key != fixtureEd25519Key {
		t.Fatalf("ParseKeyscanOutput() key = %q", keys[1].Key)
	}
}

func TestSelectFingerprintKeyPrefersEd25519(t *testing.T) {
	keys := []HostKey{
		{Hosts: "h", Type: "ssh-rsa", Key: "AAAAB3rsa"},
		{Hosts: "h", Type: "ssh-ed25519", Key: fixtureEd25519Key},
	}
	k, ok := SelectFingerprintKey(keys)
	if !ok || k.Type != "ssh-ed25519" {
		t.Fatalf("SelectFingerprintKey() = %+v, %v; want ed25519", k, ok)
	}

	k, ok = SelectFingerprintKey(keys[:1])
	if !ok || k.Type != "ssh-rsa" {
		t.Fatalf("SelectFingerprintKey() = %+v, %v; want first key fallback", k, ok)
	}

	if _, ok := SelectFingerprintKey(nil); ok {
		t.Fatal("SelectFingerprintKey(nil) ok = true, want false")
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	cases := map[string]string{
		fixtureEd25519FP: fixtureEd25519FP,
		strings.TrimPrefix(fixtureEd25519FP, "SHA256:"): fixtureEd25519FP,
		"  " + fixtureEd25519FP + " ":                   fixtureEd25519FP,
		"":                                              "",
	}
	for in, want := range cases {
		if got := NormalizeFingerprint(in); got != want {
			t.Fatalf("NormalizeFingerprint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnownHostsHostname(t *testing.T) {
	cases := []struct {
		host string
		port int
		want string
	}{
		{"example.com", 0, "example.com"},
		{"example.com", 22, "example.com"},
		{"example.com", 2222, "[example.com]:2222"},
		{"ops@example.com", 0, "example.com"},
		{"ops@example.com", 2222, "[example.com]:2222"},
	}
	for _, c := range cases {
		if got := KnownHostsHostname(c.host, c.port); got != c.want {
			t.Fatalf("KnownHostsHostname(%q, %d) = %q, want %q", c.host, c.port, got, c.want)
		}
	}
}

func scannedKeys() []HostKey {
	return []HostKey{
		{Hosts: "example.com", Type: "ssh-ed25519", Key: fixtureEd25519Key},
		{Hosts: "example.com", Type: "ssh-rsa", Key: "AAAAB3rsa"},
	}
}

func TestUpdateKnownHostsCreatesFileAndAppendsAllKeyTypes(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	result, err := UpdateKnownHosts(path, "example.com", scannedKeys(), false)
	if err != nil {
		t.Fatalf("UpdateKnownHosts() error = %v", err)
	}
	if result != TrustAdded {
		t.Fatalf("UpdateKnownHosts() = %v, want TrustAdded", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	want := "example.com ssh-ed25519 " + fixtureEd25519Key + "\nexample.com ssh-rsa AAAAB3rsa\n"
	if string(data) != want {
		t.Fatalf("known_hosts = %q, want %q", string(data), want)
	}
}

func TestUpdateKnownHostsSameKeyIsNoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	if _, err := UpdateKnownHosts(path, "example.com", scannedKeys(), false); err != nil {
		t.Fatalf("first UpdateKnownHosts() error = %v", err)
	}
	before, _ := os.ReadFile(path)

	result, err := UpdateKnownHosts(path, "example.com", scannedKeys(), false)
	if err != nil {
		t.Fatalf("second UpdateKnownHosts() error = %v", err)
	}
	if result != TrustUnchanged {
		t.Fatalf("UpdateKnownHosts() = %v, want TrustUnchanged", result)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("no-op modified file:\nbefore=%q\nafter=%q", before, after)
	}

	// A subset of the pinned keys (server now scans only ed25519) is still
	// the same key — also a no-op.
	result, err = UpdateKnownHosts(path, "example.com", scannedKeys()[:1], false)
	if err != nil || result != TrustUnchanged {
		t.Fatalf("subset UpdateKnownHosts() = %v, %v; want TrustUnchanged, nil", result, err)
	}
}

func TestUpdateKnownHostsChangedKeyRequiresRotate(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	if _, err := UpdateKnownHosts(path, "example.com", scannedKeys(), false); err != nil {
		t.Fatalf("seed UpdateKnownHosts() error = %v", err)
	}
	before, _ := os.ReadFile(path)

	changed := []HostKey{{Hosts: "example.com", Type: "ssh-ed25519", Key: "AAAAC3DIFFERENT"}}
	_, err := UpdateKnownHosts(path, "example.com", changed, false)
	if !errors.Is(err, ErrTrust) {
		t.Fatalf("UpdateKnownHosts() error = %v, want ErrTrust", err)
	}
	// The changed-key refusal is the only error marked ErrKeyChanged: callers
	// gate their `--rotate` hints on it.
	if !errors.Is(err, ErrKeyChanged) {
		t.Fatalf("UpdateKnownHosts() error = %v, want ErrKeyChanged in the chain", err)
	}
	if !strings.Contains(err.Error(), "--rotate") {
		t.Fatalf("UpdateKnownHosts() error = %v, want --rotate hint", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("failed trust modified file:\nbefore=%q\nafter=%q", before, after)
	}
}

// I/O failures are never marked ErrKeyChanged: rotating would not fix them.
func TestUpdateKnownHostsIOFailureIsNotKeyChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KnownHostsFile)
	if err := os.Mkdir(path, 0o755); err != nil { // a directory: ReadFile fails
		t.Fatal(err)
	}
	_, err := UpdateKnownHosts(path, "example.com", scannedKeys(), false)
	if err == nil {
		t.Fatal("UpdateKnownHosts() on a directory must fail")
	}
	if errors.Is(err, ErrKeyChanged) {
		t.Fatalf("I/O failure wrongly marked ErrKeyChanged: %v", err)
	}
}

func TestUpdateKnownHostsRotateReplacesAndPreservesOtherHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	seed := "# pinned by ouvrier server trust\n" +
		"other.example ssh-ed25519 AAAAC3other\n" +
		"example.com ssh-ed25519 AAAAC3old\n" +
		"example.com ssh-rsa AAAAB3old\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}

	result, err := UpdateKnownHosts(path, "example.com", scannedKeys(), true)
	if err != nil {
		t.Fatalf("UpdateKnownHosts(rotate) error = %v", err)
	}
	if result != TrustRotated {
		t.Fatalf("UpdateKnownHosts(rotate) = %v, want TrustRotated", result)
	}
	data, _ := os.ReadFile(path)
	text := string(data)
	for _, want := range []string{
		"# pinned by ouvrier server trust",
		"other.example ssh-ed25519 AAAAC3other",
		"example.com ssh-ed25519 " + fixtureEd25519Key,
		"example.com ssh-rsa AAAAB3rsa",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rotated known_hosts missing %q:\n%s", want, text)
		}
	}
	for _, gone := range []string{"AAAAC3old", "AAAAB3old"} {
		if strings.Contains(text, gone) {
			t.Fatalf("rotated known_hosts still has old key %q:\n%s", gone, text)
		}
	}
}

func TestUpdateKnownHostsRewritesHostFieldToCanonical(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	// keyscan against a non-default port prints "[host]:port"; the caller
	// passes the canonical hostname and entries are normalized to it.
	keys := []HostKey{{Hosts: "example.com", Type: "ssh-ed25519", Key: fixtureEd25519Key}}
	if _, err := UpdateKnownHosts(path, "[example.com]:2222", keys, false); err != nil {
		t.Fatalf("UpdateKnownHosts() error = %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "[example.com]:2222 ssh-ed25519 ") {
		t.Fatalf("known_hosts = %q, want canonical [host]:port field", string(data))
	}
	trusted, err := HostTrusted(path, "[example.com]:2222")
	if err != nil || !trusted {
		t.Fatalf("HostTrusted() = %v, %v; want true", trusted, err)
	}
}

func TestUpdateKnownHostsRejectsEmptyScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), KnownHostsFile)
	if _, err := UpdateKnownHosts(path, "example.com", nil, false); !errors.Is(err, ErrTrust) {
		t.Fatalf("UpdateKnownHosts(nil keys) error = %v, want ErrTrust", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty scan must not create the file; stat = %v", statErr)
	}
}

func TestHostTrusted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KnownHostsFile)

	trusted, err := HostTrusted(path, "example.com")
	if err != nil || trusted {
		t.Fatalf("HostTrusted(missing file) = %v, %v; want false, nil", trusted, err)
	}

	content := "example.com,alias.example ssh-ed25519 " + fixtureEd25519Key + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	for _, host := range []string{"example.com", "alias.example", "EXAMPLE.COM"} {
		trusted, err := HostTrusted(path, host)
		if err != nil || !trusted {
			t.Fatalf("HostTrusted(%q) = %v, %v; want true, nil", host, trusted, err)
		}
	}
	trusted, err = HostTrusted(path, "other.example")
	if err != nil || trusted {
		t.Fatalf("HostTrusted(other) = %v, %v; want false, nil", trusted, err)
	}
}
