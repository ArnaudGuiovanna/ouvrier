package operate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	TranscriptPath     string       `json:"transcript_path,omitempty"`
	EventsPath         string       `json:"events_path,omitempty"`
	ToolCallsPath      string       `json:"tool_calls_path,omitempty"`
	AuthProfilePath    string       `json:"auth_profile_path,omitempty"`
	CheckpointsDir     string       `json:"checkpoints_dir,omitempty"`
	ExportPath         string       `json:"export_path,omitempty"`
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
	root        string
	projectRoot string
	redactor    Redactor
	now         func() time.Time
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
	rootAbs, rootReal, err := realDirectory(abs)
	if err != nil {
		return nil, fmt.Errorf("operate: resolve project dir: %w", err)
	}
	if rootAbs != rootReal {
		// Keep every persisted path anchored to the canonical workspace. A
		// symlink supplied as the initial root would otherwise make later
		// confinement checks depend on mutable filesystem state.
		abs = rootReal
	}
	root, err := ensureWorkerStateDir(abs, "operate")
	if err != nil {
		return nil, err
	}
	return &Store{root: root, projectRoot: rootReal, now: time.Now}, nil
}

// Root returns the .ouvrier/operate directory for this store.
func (s *Store) Root() string { return s.root }

// SessionDir returns the directory that holds one session's artifacts.
func (s *Store) SessionDir(id string) string {
	if !validSessionID(id) {
		return ""
	}
	return filepath.Join(s.root, "sessions", id)
}

// LatestSessionID returns the id of the most recently updated session, by the
// mtime of its session.json. It returns an error when no session exists.
func (s *Store) LatestSessionID() (string, error) {
	dir := filepath.Join(s.root, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("operate: list sessions: %w", err)
	}
	var latestID string
	var latestMod time.Time
	for _, entry := range entries {
		if !entry.IsDir() || !validSessionID(entry.Name()) {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name(), "session.json"))
		if err != nil {
			continue
		}
		if info.ModTime().After(latestMod) {
			latestMod = info.ModTime()
			latestID = entry.Name()
		}
	}
	if latestID == "" {
		return "", fmt.Errorf("operate: no sessions found")
	}
	return latestID, nil
}

// Create starts a durable session and writes session.json immediately.
func (s *Store) Create(projectDir, driver, codexMode string) (*Session, error) {
	id, err := randomID()
	if err != nil {
		return nil, err
	}
	abs, err := s.workspaceDir(projectDir)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	dir, err := s.secureSessionDir(id, true)
	if err != nil {
		return nil, err
	}
	session := &Session{
		ID:              id,
		Status:          StatusNew,
		Dir:             abs,
		GoalPath:        filepath.Join(dir, "goal.md"),
		PlanPath:        filepath.Join(dir, "plan.md"),
		PatchPath:       filepath.Join(dir, "patch.json"),
		DiffPath:        filepath.Join(dir, "diff.patch"),
		AuditPath:       filepath.Join(dir, "audit.json"),
		ReviewPath:      filepath.Join(dir, "review.json"),
		BuildPath:       filepath.Join(dir, "build.json"),
		TransferPath:    filepath.Join(dir, "transfer.json"),
		TranscriptPath:  filepath.Join(dir, "transcript.jsonl"),
		EventsPath:      filepath.Join(dir, "events.jsonl"),
		ToolCallsPath:   filepath.Join(dir, "tool-calls.jsonl"),
		AuthProfilePath: filepath.Join(dir, "auth_profile.json"),
		CheckpointsDir:  filepath.Join(dir, "checkpoints"),
		ExportPath:      filepath.Join(dir, "export.md"),
		Driver:          driver,
		CodexMode:       codexMode,
		CreatedAt:       now,
		UpdatedAt:       now,
		Transitions:     []Transition{{At: now, To: StatusNew, Reason: "created"}},
	}
	if err := s.Save(session); err != nil {
		return nil, err
	}
	return session, nil
}

// Load reads a persisted session.
func (s *Store) Load(id string) (*Session, error) {
	if !validSessionID(id) {
		return nil, fmt.Errorf("operate: invalid session id %q", id)
	}
	dir, err := s.secureSessionDir(id, false)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "session.json")
	file, err := openSessionArtifact(path, os.O_RDONLY, 0, false)
	if err != nil {
		return nil, fmt.Errorf("operate: open session %s: %w", id, err)
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("operate: read session %s: %w", id, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("operate: close session %s: %w", id, closeErr)
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("operate: parse session %s: %w", id, err)
	}
	if session.ID != id || !validSessionID(session.ID) {
		return nil, fmt.Errorf("operate: persisted session id %q does not match requested id %q", session.ID, id)
	}
	resolvedDir, err := s.workspaceDir(session.Dir)
	if err != nil {
		return nil, fmt.Errorf("operate: invalid workspace in session %s: %w", id, err)
	}
	session.Dir = resolvedDir
	if session.CandidateDir != "" {
		candidate, resolveErr := s.workspaceDir(session.CandidateDir)
		if resolveErr != nil {
			return nil, fmt.Errorf("operate: invalid candidate workspace in session %s: %w", id, resolveErr)
		}
		session.CandidateDir = candidate
	}
	session.setArtifactPaths(dir)
	return &session, nil
}

// Save atomically writes session.json.
func (s *Store) Save(session *Session) error {
	if session == nil {
		return errors.New("operate: nil session")
	}
	if !validSessionID(session.ID) {
		return fmt.Errorf("operate: invalid session id %q", session.ID)
	}
	dir, err := s.secureSessionDir(session.ID, true)
	if err != nil {
		return err
	}
	workspace, err := s.workspaceDir(session.Dir)
	if err != nil {
		return fmt.Errorf("operate: invalid session workspace: %w", err)
	}
	session.Dir = workspace
	if session.CandidateDir != "" {
		candidate, resolveErr := s.workspaceDir(session.CandidateDir)
		if resolveErr != nil {
			return fmt.Errorf("operate: invalid candidate workspace: %w", resolveErr)
		}
		session.CandidateDir = candidate
	}
	session.setArtifactPaths(dir)
	session.UpdatedAt = s.now().UTC()
	// Persist only a redacted copy. The in-memory session remains useful to the
	// caller, while command/driver errors and operator text cannot leak through
	// session.json.
	persisted := *session
	persisted.LastError = s.redactor.Redact(persisted.LastError)
	persisted.OverrideRationale = s.redactor.Redact(persisted.OverrideRationale)
	persisted.AcceptedRiskReason = s.redactor.Redact(persisted.AcceptedRiskReason)
	persisted.Branch = s.redactor.Redact(persisted.Branch)
	persisted.Transitions = append([]Transition(nil), persisted.Transitions...)
	for i := range persisted.Transitions {
		persisted.Transitions[i].Reason = s.redactor.Redact(persisted.Transitions[i].Reason)
	}
	data, err := json.MarshalIndent(&persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode session: %w", err)
	}
	data = append(data, '\n')
	return writeAtomic(filepath.Join(dir, "session.json"), data, 0o600)
}

func (s *Session) setArtifactPaths(dir string) {
	s.GoalPath = filepath.Join(dir, "goal.md")
	s.PlanPath = filepath.Join(dir, "plan.md")
	s.PatchPath = filepath.Join(dir, "patch.json")
	s.DiffPath = filepath.Join(dir, "diff.patch")
	s.AuditPath = filepath.Join(dir, "audit.json")
	s.ReviewPath = filepath.Join(dir, "review.json")
	s.BuildPath = filepath.Join(dir, "build.json")
	s.TransferPath = filepath.Join(dir, "transfer.json")
	s.TranscriptPath = filepath.Join(dir, "transcript.jsonl")
	s.EventsPath = filepath.Join(dir, "events.jsonl")
	s.ToolCallsPath = filepath.Join(dir, "tool-calls.jsonl")
	s.AuthProfilePath = filepath.Join(dir, "auth_profile.json")
	s.CheckpointsDir = filepath.Join(dir, "checkpoints")
	s.ExportPath = filepath.Join(dir, "export.md")
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
	if !validSessionID(session.ID) {
		return "", fmt.Errorf("operate: invalid session id %q", session.ID)
	}
	if strings.TrimSpace(name) == "" || name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("operate: artifact name %q must be a base name", name)
	}
	dir, err := s.secureSessionDir(session.ID, false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := writeAtomic(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func validSessionID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func (s *Store) secureSessionDir(id string, create bool) (string, error) {
	if !validSessionID(id) {
		return "", fmt.Errorf("operate: invalid session id %q", id)
	}
	sessions, err := ensureWorkerStateDir(s.projectRoot, "operate/sessions")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(sessions, id)
	info, statErr := os.Lstat(dir)
	if statErr != nil {
		if !os.IsNotExist(statErr) {
			return "", fmt.Errorf("operate: inspect session dir: %w", statErr)
		}
		if !create {
			return "", fmt.Errorf("operate: session %s does not exist: %w", id, os.ErrNotExist)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", fmt.Errorf("operate: create session dir: %w", err)
		}
		info, statErr = os.Lstat(dir)
	}
	if statErr != nil {
		return "", fmt.Errorf("operate: inspect session dir: %w", statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("operate: session dir must be a real directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("operate: protect session dir: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !pathWithinRoot(sessions, resolved) {
		return "", fmt.Errorf("operate: session dir escapes the session store")
	}
	return filepath.Clean(resolved), nil
}

func (s *Store) workspaceDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("workspace directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	real = filepath.Clean(real)
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("workspace %q is not a directory", real)
	}
	return real, nil
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("operate: generate session id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	return writeAtomicStream(path, mode, func(writer io.Writer) error {
		_, err := writer.Write(data)
		return err
	})
}

func writeAtomicStream(path string, mode os.FileMode, write func(io.Writer) error) error {
	return writeAnchoredAtomicStream(path, mode, write)
}
