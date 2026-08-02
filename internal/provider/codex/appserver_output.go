package codex

import (
	"fmt"
	"strings"
)

func (t *appServerTurn) appendAgentDelta(itemID, delta string) error {
	if t == nil || delta == "" {
		return nil
	}
	builder := t.deltas[itemID]
	extra := len(delta)
	if builder == nil {
		if len(t.deltaIDs)+len(t.text) >= maxAppServerTextItems {
			return fmt.Errorf("codex app-server response exceeds %d text items", maxAppServerTextItems)
		}
		builder = &strings.Builder{}
		t.deltas[itemID] = builder
		t.deltaIDs = append(t.deltaIDs, itemID)
		extra++ // newline/join overhead budget for this item
	}
	if extra > maxAppServerTextBytes-t.textCost {
		return fmt.Errorf("codex app-server response text exceeds %d bytes", maxAppServerTextBytes)
	}
	builder.WriteString(delta)
	t.textCost += extra
	return nil
}

func (t *appServerTurn) completeAgentItem(itemID, itemType, authoritative string) error {
	if t == nil {
		return nil
	}
	delta, hadDelta := t.dropAgentDelta(itemID)
	if itemType != "agentMessage" {
		if hadDelta {
			return fmt.Errorf("codex app-server agent delta completed as unexpected item type %q", itemType)
		}
		return nil
	}
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("codex app-server completed agent message omitted item id")
	}
	text := authoritative
	if strings.TrimSpace(text) == "" {
		text = delta
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if len(t.text) >= maxAppServerTextItems {
		return fmt.Errorf("codex app-server response exceeds %d text items", maxAppServerTextItems)
	}
	cost := len(text) + 1
	if cost > maxAppServerTextBytes-t.textCost {
		return fmt.Errorf("codex app-server response text exceeds %d bytes", maxAppServerTextBytes)
	}
	t.text = append(t.text, text)
	t.textCost += cost
	return nil
}

func (t *appServerTurn) dropAgentDelta(itemID string) (string, bool) {
	builder, ok := t.deltas[itemID]
	if !ok {
		return "", false
	}
	delete(t.deltas, itemID)
	text := builder.String()
	t.textCost -= len(text) + 1
	if t.textCost < 0 {
		t.textCost = 0
	}
	for i, id := range t.deltaIDs {
		if id == itemID {
			copy(t.deltaIDs[i:], t.deltaIDs[i+1:])
			t.deltaIDs = t.deltaIDs[:len(t.deltaIDs)-1]
			break
		}
	}
	return text, true
}

func (t *appServerTurn) takeAgentText() string {
	if t == nil {
		return ""
	}
	for _, itemID := range t.deltaIDs {
		if delta := t.deltas[itemID]; delta != nil && strings.TrimSpace(delta.String()) != "" {
			t.text = append(t.text, delta.String())
		}
	}
	t.deltas = make(map[string]*strings.Builder)
	t.deltaIDs = nil
	text := strings.TrimSpace(strings.Join(t.text, "\n"))
	t.text = nil
	t.textCost = 0
	return text
}
