package operate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Session is the human-readable durable state for one operate run.
type Session struct {
	ID                 string       `json:"id"`
	Status             Status       `json:"status"`
	Dir                string       `json:"dir"`
	CandidateDir       string       `json:"candidate_dir,omitempty"`
	Branch             string       `json:"branch,omitempty"`
	GoalPath           string       `json:"goal_path,omitempty"`
	PlanPath           string       `json:"plan_path,omitempty"`
	PatchPath          string       `json:"patch_path,omitempty"`
	DiffPath           string       `json:"diff_path,omitempty"`
	AuditPath          string       `json:"audit_path,omitempty"`
	ReviewPath         string       `json:"review_path,omitempty"`
	BuildPath          string       `json:"build_path,omitempty"`
	TransferPath       string       `json:"transfer_path,omitempty"`
	Driver             string       `json:"driver,omitempty"`
	CodexMode          string       `json:"codex_mode,omitempty"`
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
	Transitions        []Transition `json:"transitions"`
	LastError          string       `json:"last_error,omitempty"`
	OverrideRationale  string       `json:"override_rationale,omitempty"`
	AcceptedRiskReason string       `json:"accepted_risk_reason,omitempty"`
}

// Transition records one append-only state change.
type Transition struct {
	At     time.Time `json:"at"`
	From   Status    `json:"from,omitempty"`
	To     Status    `json:"to"`
	Reason string    `json:"reason,omitempty"`
}

// Store persists operate sessions under .ouvrier/operate/sessions.
type Store struct {
	root string
	now  func() time.Time
}

// NewStore returns a session store rooted in projectDir/.ouvrier/operate.
func NewStore(projectDir string) (*Store, error) {
	if projectDir == "" {
		projectDir = "."
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve project dir: %w", err)
	}
	return &Store{root: filepath.Join(abs, ".ouvrier", "operate"), now: time.Now}, nil
}

// Root returns the .ouvrier/operate directory for this store.
func (s *Store) Root() string { return s.root }

// SessionDir returns the directory that holds one session's artifacts.
func (s *Store) SessionDir(id string) string {
	return filepath.Join(s.root, "sessions", id)
}

// Create starts a durable session and writes session.json immediately.
func (s *Store) Create(projectDir, driver, codexMode string) (*Session, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve project dir: %w", err)
	}
	now := s.now().UTC()
	dir := s.SessionDir(id)
	session := &Session{
		ID:           id,
		Status:       StatusNew,
		Dir:          abs,
		GoalPath:     filepath.Join(dir, "goal.md"),
		PlanPath:     filepath.Join(dir, "plan.md"),
		PatchPath:    filepath.Join(dir, "patch.json"),
		DiffPath:     filepath.Join(dir, "diff.patch"),
		AuditPath:    filepath.Join(dir, "audit.json"),
		ReviewPath:   filepath.Join(dir, "review.json"),
		BuildPath:    filepath.Join(dir, "build.json"),
		TransferPath: filepath.Join(dir, "transfer.json"),
		Driver:       driver,
		CodexMode:    codexMode,
		CreatedAt:    now,
		UpdatedAt:    now,
		Transitions:  []Transition{{At: now, To: StatusNew, Reason: "created"}},
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("operate: create session dir: %w", err)
	}
	if err := s.Save(session); err != nil {
		return nil, err
	}
	return session, nil
}

// Load reads a persisted session.
func (s *Store) Load(id string) (*Session, error) {
	if id == "" {
		return nil, errors.New("operate: session id is required")
	}
	data, err := os.ReadFile(filepath.Join(s.SessionDir(id), "session.json"))
	if err != nil {
		return nil, fmt.Errorf("operate: read session %s: %w", id, err)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("operate: parse session %s: %w", id, err)
	}
	return &session, nil
}

// Save atomically writes session.json.
func (s *Store) Save(session *Session) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	dir := s.SessionDir(session.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("operate: create session dir: %w", err)
	}
	session.UpdatedAt = s.now().UTC()
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode session: %w", err)
	}
	data = append(data, '\n')
	return writeAtomic(filepath.Join(dir, "session.json"), data, 0o600)
}

// Transition moves a session to status and persists the change.
func (s *Store) Transition(session *Session, status Status, reason string) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	now := s.now().UTC()
	from := session.Status
	session.Status = status
	session.Transitions = append(session.Transitions, Transition{
		At:     now,
		From:   from,
		To:     status,
		Reason: reason,
	})
	return s.Save(session)
}

// WriteArtifact stores one session artifact relative to the session directory.
func (s *Store) WriteArtifact(session *Session, name string, data []byte) (string, error) {
	if session == nil {
		return "", errors.New("operate: nil session")
	}
	if filepath.Base(name) != name {
		return "", fmt.Errorf("operate: artifact name %q must be a base name", name)
	}
	path := filepath.Join(s.SessionDir(session.ID), name)
	if err := writeAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("operate: generate session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("operate: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("operate: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("operate: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("operate: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("operate: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("operate: replace %s: %w", path, err)
	}
	return nil
}
