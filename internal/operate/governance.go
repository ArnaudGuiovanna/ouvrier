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
