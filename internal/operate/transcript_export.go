package operate

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

const maxSessionExportBytes = 128 << 20

func exportTranscriptMarkdown(ctx context.Context, session *Session, destination string) error {
	if session == nil {
		return fmt.Errorf("operate: export requires a session")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return writeAtomicStream(destination, 0o600, func(raw io.Writer) error {
		limited := &sessionExportWriter{writer: raw, remaining: maxSessionExportBytes}
		writer := bufio.NewWriterSize(limited, 64<<10)
		if _, err := fmt.Fprintf(writer, "# Ouvrier operate session %s\n\n", session.ID); err != nil {
			return err
		}
		if err := streamTranscriptEntries(ctx, session.TranscriptPath, func(entry TranscriptEntry) error {
			return writeTranscriptMarkdownEntry(writer, entry)
		}); err != nil {
			return err
		}
		return writer.Flush()
	})
}

func streamTranscriptEntries(ctx context.Context, path string, visit func(TranscriptEntry) error) error {
	if visit == nil {
		return fmt.Errorf("operate: transcript visitor is nil")
	}
	file, err := openSessionArtifact(path, os.O_RDONLY, 0, false)
	if err != nil {
		return fmt.Errorf("operate: open transcript: %w", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() > maxTranscriptFileBytes {
		return fmt.Errorf("operate: transcript is not a bounded regular file")
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), maxJSONLLineBytes+1)
	line := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line++
		if line > maxTranscriptEntries {
			return fmt.Errorf("operate: transcript exceeds %d entries", maxTranscriptEntries)
		}
		var entry TranscriptEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return fmt.Errorf("operate: parse transcript at line %d: %w", line, err)
		}
		if err := visit(entry); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("operate: read transcript: %w", err)
	}
	return nil
}

func writeTranscriptMarkdownEntry(writer io.Writer, entry TranscriptEntry) error {
	switch entry.Kind {
	case TranscriptUser:
		_, err := fmt.Fprintf(writer, "## User\n\n%s\n\n", entry.Text)
		return err
	case TranscriptAssistant:
		_, err := fmt.Fprintf(writer, "## Agent\n\n%s\n\n", entry.Text)
		return err
	case TranscriptToolCall:
		return writeTranscriptJSONBlock(writer, "Tool Call", entry.ToolName, entry.Input)
	case TranscriptToolResult:
		return writeTranscriptJSONBlock(writer, "Tool Result", entry.ToolName, entry.Output)
	case TranscriptError:
		_, err := fmt.Fprintf(writer, "## Error\n\n%s\n\n", entry.Text)
		return err
	case TranscriptStatus:
		_, err := fmt.Fprintf(writer, "## Status\n\n%s\n\n", entry.Text)
		return err
	default:
		return nil
	}
}

func writeTranscriptJSONBlock(writer io.Writer, label, tool string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("operate: encode transcript export: %w", err)
	}
	_, err = fmt.Fprintf(writer, "## %s: %s\n\n```json\n%s\n```\n\n", label, tool, encoded)
	return err
}

type sessionExportWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *sessionExportWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, fmt.Errorf("operate: session export exceeds %d bytes", maxSessionExportBytes)
	}
	written, err := w.writer.Write(data)
	w.remaining -= int64(written)
	return written, err
}
