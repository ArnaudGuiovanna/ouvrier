package ovr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (rt httpRuntime) serveAdminEvents(w http.ResponseWriter, req *http.Request) {
	if !rt.authorizeAdmin(w, req) {
		return
	}
	afterID, err := parseAdminTraceAfterID(req.URL.Query().Get("after_id"))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, "invalid_event_cursor")
		return
	}
	format := adminEventStreamFormat(req)
	follow := !strings.EqualFold(strings.TrimSpace(req.URL.Query().Get("follow")), "false")

	flusher, canFlush := w.(http.Flusher)
	if follow && !canFlush {
		writeJSONStatus(w, http.StatusInternalServerError, "streaming_not_supported")
		return
	}

	switch format {
	case "sse":
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	if err := rt.writeAdminEventStreamBatch(req.Context(), w, format, &afterID); err != nil {
		return
	}
	if !follow {
		if canFlush {
			flusher.Flush()
		}
		return
	}
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-ticker.C:
		}
		if err := rt.writeAdminEventStreamBatch(req.Context(), w, format, &afterID); err != nil {
			return
		}
		flusher.Flush()
	}
}

func adminEventStreamFormat(req *http.Request) string {
	format := strings.ToLower(strings.TrimSpace(req.URL.Query().Get("format")))
	if format == "sse" || strings.Contains(strings.ToLower(req.Header.Get("Accept")), "text/event-stream") {
		return "sse"
	}
	return "jsonl"
}

func (rt httpRuntime) writeAdminEventStreamBatch(ctx context.Context, w ioWriter, format string, afterID *uint64) error {
	recorded, err := rt.adminEventsSince(ctx, "", *afterID)
	if err != nil {
		return err
	}
	for _, event := range recorded {
		if event.ID > *afterID {
			*afterID = event.ID
		}
		if err := writeAdminEventStreamRecord(w, format, adminEventResponseFromEvent(event)); err != nil {
			return err
		}
	}
	return nil
}

type ioWriter interface {
	Write([]byte) (int, error)
}

func writeAdminEventStreamRecord(w ioWriter, format string, event adminEventResponse) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if format == "sse" {
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.ID, event.Kind, encoded); err != nil {
			return err
		}
		return nil
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}
