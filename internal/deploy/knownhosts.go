package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// KnownHostsFile is the project-root file holding pinned SSH host keys. It is
// committed alongside pip.yaml: host public keys are not secrets, and pinning
// them in the repository makes the trust decision team-shared and CI-trivial.
const KnownHostsFile = "ouvrier.known_hosts"

// ErrTrust is returned when `ouvrier server trust` cannot proceed (keyscan
// failures, fingerprint mismatches, or a changed key without --rotate).
var ErrTrust = errors.New("trust error")

// ErrKeyChanged marks the specific UpdateKnownHosts refusal where the host is
// already pinned with a different key. Callers gate their "rerun with
// --rotate" hints on it so I/O failures are never decorated with rotation
// advice.
var ErrKeyChanged = errors.New("host key changed")

// KeyscanRunner is the ssh-keyscan seam, mirroring GoRunner/RemoteRunner so
// tests can substitute canned output without a network.
type KeyscanRunner func(ctx context.Context, host string, port int) (output string, err error)

// DefaultKeyscan shells out to the system ssh-keyscan binary.
func DefaultKeyscan(ctx context.Context, host string, port int) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasPrefix(host, "-") {
		return "", fmt.Errorf("%w: invalid keyscan host %q", ErrTrust, host)
	}
	args := []string{"-T", "5"}
	if port != 0 {
		args = append(args, "-p", strconv.Itoa(port))
	}
	args = append(args, host)
	stdout, _, err := runHostCommand(ctx, "ssh-keyscan", args, nil)
	if err != nil {
		return "", fmt.Errorf("%w: ssh-keyscan %s: %w", ErrTrust, host, err)
	}
	return stdout, nil
}

// HostKey is one parsed ssh-keyscan output line / known_hosts entry.
type HostKey struct {
	Hosts string // host field, e.g. "example.com" or "[example.com]:2222"
	Type  string // key algorithm, e.g. ssh-ed25519
	Key   string // base64-encoded key material
}

// Line renders the entry back into known_hosts format.
func (k HostKey) Line() string { return k.Hosts + " " + k.Type + " " + k.Key }

// matchesHost reports whether this entry pins the given canonical hostname.
// The host field of a known_hosts line may be a comma-separated list.
func (k HostKey) matchesHost(host string) bool {
	for _, h := range strings.Split(k.Hosts, ",") {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// ParseKeyscanOutput parses ssh-keyscan stdout (also valid known_hosts
// content) into entries, skipping blanks and # comments.
func ParseKeyscanOutput(output string) []HostKey {
	var keys []HostKey
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		keys = append(keys, HostKey{Hosts: fields[0], Type: fields[1], Key: fields[2]})
	}
	return keys
}

// Fingerprint computes the standard OpenSSH SHA256 fingerprint of a
// base64-encoded public key: sha256 over the raw key bytes, rendered as
// "SHA256:" plus unpadded base64 — the same form `ssh-keygen -lf` prints.
func Fingerprint(keyB64 string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return "", fmt.Errorf("%w: decode public key: %w", ErrTrust, err)
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// SelectFingerprintKey picks the single key whose fingerprint is displayed
// and verified by `ouvrier server trust`: the ed25519 key when the server
// offers one, otherwise the first scanned key. All scanned key lines are
// pinned either way; this only selects which one the human (or --fingerprint)
// confirms.
func SelectFingerprintKey(keys []HostKey) (HostKey, bool) {
	if len(keys) == 0 {
		return HostKey{}, false
	}
	for _, k := range keys {
		if k.Type == "ssh-ed25519" {
			return k, true
		}
	}
	return keys[0], true
}

// NormalizeFingerprint accepts a fingerprint with or without the "SHA256:"
// prefix and returns the canonical prefixed form. Comparison stays
// case-sensitive on the base64 payload.
func NormalizeFingerprint(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "SHA256:") {
		return s
	}
	return "SHA256:" + s
}

// KnownHostsHostname canonicalizes a host for known_hosts lookup the way ssh
// does: a user@ prefix is stripped, and a non-default port wraps the host as
// "[host]:port".
func KnownHostsHostname(host string, port int) string {
	host = strings.TrimSpace(host)
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if port != 0 && port != 22 {
		return fmt.Sprintf("[%s]:%d", host, port)
	}
	return host
}

// HostTrusted reports whether the known_hosts file at path pins any key for
// the canonical hostname. A missing file means nothing is trusted.
func HostTrusted(path, host string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: read %s: %w", ErrTrust, path, err)
	}
	for _, k := range ParseKeyscanOutput(string(data)) {
		if k.matchesHost(host) {
			return true, nil
		}
	}
	return false, nil
}

// TrustResult describes what UpdateKnownHosts changed.
type TrustResult int

const (
	// TrustAdded means the host had no entry and its keys were appended.
	TrustAdded TrustResult = iota
	// TrustUnchanged means every scanned key was already pinned (no write).
	TrustUnchanged
	// TrustRotated means existing entries were replaced under --rotate.
	TrustRotated
)

// UpdateKnownHosts pins the scanned keys for the canonical hostname in the
// known_hosts file at path, creating the file when missing and preserving
// entries for other hosts verbatim.
//
//   - No existing entry: all key lines are appended (TrustAdded).
//   - Every scanned key already pinned: friendly no-op (TrustUnchanged).
//   - Any scanned key differs and rotate is false: error naming --rotate,
//     nothing written.
//   - rotate true: all existing lines for the host are replaced.
func UpdateKnownHosts(path, host string, keys []HostKey, rotate bool) (TrustResult, error) {
	if len(keys) == 0 {
		return TrustUnchanged, fmt.Errorf("%w: no host keys to pin for %s", ErrTrust, host)
	}
	for i := range keys {
		keys[i].Hosts = host
	}

	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return TrustUnchanged, fmt.Errorf("%w: read %s: %w", ErrTrust, path, err)
	}

	var kept []string
	existing := map[string]bool{} // "type key" pairs already pinned for host
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		entry := ParseKeyscanOutput(line)
		if len(entry) == 1 && entry[0].matchesHost(host) {
			existing[entry[0].Type+" "+entry[0].Key] = true
			if rotate {
				continue // dropped: replaced by the fresh scan below
			}
			kept = append(kept, line)
			continue
		}
		kept = append(kept, line)
	}
	// Drop trailing blank lines so appends stay tidy.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	result := TrustAdded
	if len(existing) > 0 {
		allPinned := true
		for _, k := range keys {
			if !existing[k.Type+" "+k.Key] {
				allPinned = false
				break
			}
		}
		if allPinned && !rotate {
			return TrustUnchanged, nil
		}
		if !rotate {
			return TrustUnchanged, fmt.Errorf(
				"%w: %w: %s is already trusted with a different key; rerun with --rotate to replace the pinned entry (nothing written)",
				ErrTrust, ErrKeyChanged, host,
			)
		}
		result = TrustRotated
	}

	for _, k := range keys {
		kept = append(kept, k.Line())
	}
	content := strings.Join(kept, "\n") + "\n"
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return TrustUnchanged, fmt.Errorf("%w: create %s: %w", ErrTrust, dir, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return TrustUnchanged, fmt.Errorf("%w: write %s: %w", ErrTrust, tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return TrustUnchanged, fmt.Errorf("%w: rename %s: %w", ErrTrust, path, err)
	}
	return result, nil
}
