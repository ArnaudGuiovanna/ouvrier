package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is a single parsed Server-Sent Events frame.
type sseEvent struct {
	Event string
	Data  string
}

// scanSSE reads SSE frames from r, invoking fn for each complete event frame
// (delimited by a blank line). Multiple data: lines in one frame are joined
// with newlines, matching the SSE spec. fn returning false stops scanning.
func scanSSE(r io.Reader, fn func(sseEvent) bool) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var event string
	var data []string
	flush := func() bool {
		if len(data) == 0 && event == "" {
			return true
		}
		ev := sseEvent{Event: event, Data: strings.Join(data, "\n")}
		event = ""
		data = data[:0]
		return fn(ev)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if !flush() {
				return scanner.Err()
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			field = line
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return nil
}
