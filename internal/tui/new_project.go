package tui

import (
	tea "charm.land/bubbletea/v2"
)

// NewProjectModel returns the Bubble Tea model that backs `ouvrier new`.
// It is kept as a thin factory so callers (and tests) can introspect the
// wizard without knowing the concrete implementation type.
func NewProjectModel() tea.Model {
	return NewProjectWizard(NewProjectWizardOptions{})
}

func clamp(value int, minValue int, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
