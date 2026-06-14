package operate

// Governance mirrors the Ouvrier framework's own tool governance levels and
// drives the operate approval gate.
type Governance string

const (
	GovReadOnly         Governance = "read_only"
	GovIdempotent       Governance = "idempotent"
	GovSideEffecting    Governance = "side_effecting"
	GovRequiresApproval Governance = "requires_approval"
)

// Posture is the operator's standing approval stance, cycled with Shift+Tab.
type Posture string

const (
	PostureManual   Posture = "manual"
	PostureAutoSafe Posture = "auto-safe"
	PosturePlan     Posture = "plan"
)

// NeedsApproval reports whether a tool with this governance must prompt the
// operator under the given posture. RequiresApproval always prompts; the posture
// can only narrow what auto-passes, never weaken this floor.
func (g Governance) NeedsApproval(p Posture) bool {
	switch g {
	case GovReadOnly, GovIdempotent:
		return false
	case GovRequiresApproval:
		return true
	case GovSideEffecting:
		return p != PostureAutoSafe
	default:
		return true
	}
}

// SideEffecting reports whether the governance performs any write/build/deploy.
func (g Governance) SideEffecting() bool {
	return g == GovSideEffecting || g == GovRequiresApproval
}

// ApprovalRequest is emitted before a governed tool runs; the operator answers
// with an ApprovalDecision carrying the same ID.
type ApprovalRequest struct {
	ID         string         `json:"id"`
	Tool       string         `json:"tool"`
	Governance Governance     `json:"governance"`
	Summary    string         `json:"summary"`
	Prod       bool           `json:"prod,omitempty"`
	Details    map[string]any `json:"details,omitempty"`
}

// ApprovalDecision is the operator's answer to an ApprovalRequest.
type ApprovalDecision struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Reason   string `json:"reason,omitempty"`
}

// turnControl carries per-turn operator context into the tool loop.
type turnControl struct {
	posture     Posture
	decisions   <-chan ApprovalDecision
	interactive bool
}

func headlessControl() *turnControl { return &turnControl{posture: PostureManual, interactive: false} }
