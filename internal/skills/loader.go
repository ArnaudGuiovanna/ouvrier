package skills

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"ouvrier/internal/sandbox"
)

const (
	dirName      = "skills"
	fileName     = "SKILL.md"
	maxSkillSize = 256 << 10
)

var ErrInvalidSkill = errors.New("invalid skill")

type LoadedSkill struct {
	Name        string
	Description string
	Body        string
	Path        string
}

func Load(ctx context.Context, workspace *sandbox.Sandbox, name string) (LoadedSkill, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return LoadedSkill{}, err
	}
	name = strings.TrimSpace(name)
	if err := validateName(name); err != nil {
		return LoadedSkill{}, err
	}
	if workspace == nil {
		return LoadedSkill{}, fmt.Errorf("%w: workspace sandbox is required", ErrInvalidSkill)
	}

	path, err := workspace.Resolve(filepath.Join(dirName, name, fileName))
	if err != nil {
		return LoadedSkill{}, fmt.Errorf("%w: resolve %s/%s/%s: %w", ErrInvalidSkill, dirName, name, fileName, err)
	}
	raw, err := readBounded(path)
	if err != nil {
		return LoadedSkill{}, err
	}
	parsed, err := parseSkill(name, raw)
	if err != nil {
		return LoadedSkill{}, err
	}
	parsed.Path = path
	return parsed, nil
}

func validateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: skill name is required", ErrInvalidSkill)
	case name == "." || name == "..":
		return fmt.Errorf("%w: skill name must be a direct child of skills", ErrInvalidSkill)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: skill name must not contain path separators", ErrInvalidSkill)
	default:
		return nil
	}
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrInvalidSkill, path, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxSkillSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrInvalidSkill, path, err)
	}
	if len(data) > maxSkillSize {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidSkill, path, maxSkillSize)
	}
	return data, nil
}

func parseSkill(expectedName string, raw []byte) (LoadedSkill, error) {
	frontmatter, body, ok := splitFrontmatter(raw)
	if !ok {
		return LoadedSkill{}, fmt.Errorf("%w: SKILL.md must start with frontmatter", ErrInvalidSkill)
	}
	fields := parseFrontmatterFields(frontmatter)
	name := strings.TrimSpace(fields["name"])
	description := strings.TrimSpace(fields["description"])
	switch {
	case name == "":
		return LoadedSkill{}, fmt.Errorf("%w: frontmatter name is required", ErrInvalidSkill)
	case description == "":
		return LoadedSkill{}, fmt.Errorf("%w: frontmatter description is required", ErrInvalidSkill)
	case name != expectedName:
		return LoadedSkill{}, fmt.Errorf("%w: frontmatter name %q does not match skill %q", ErrInvalidSkill, name, expectedName)
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return LoadedSkill{}, fmt.Errorf("%w: skill body is required", ErrInvalidSkill)
	}
	return LoadedSkill{Name: name, Description: description, Body: string(body)}, nil
}

func splitFrontmatter(raw []byte) ([]byte, []byte, bool) {
	firstLine, offset := nextLine(raw, 0)
	if strings.TrimSpace(string(firstLine)) != "---" {
		return nil, nil, false
	}

	frontmatterStart := offset
	for offset < len(raw) {
		lineStart := offset
		line, next := nextLine(raw, offset)
		if strings.TrimSpace(string(line)) == "---" {
			return raw[frontmatterStart:lineStart], raw[next:], true
		}
		offset = next
	}
	return nil, nil, false
}

func nextLine(raw []byte, offset int) ([]byte, int) {
	if offset >= len(raw) {
		return nil, len(raw)
	}
	end := bytes.IndexByte(raw[offset:], '\n')
	if end < 0 {
		line := bytes.TrimSuffix(raw[offset:], []byte("\r"))
		return line, len(raw)
	}
	lineEnd := offset + end
	line := bytes.TrimSuffix(raw[offset:lineEnd], []byte("\r"))
	return line, lineEnd + 1
}

func parseFrontmatterFields(raw []byte) map[string]string {
	fields := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		fields[key] = trimYAMLScalar(value)
	}
	return fields
}

func trimYAMLScalar(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return strings.TrimSpace(value[1 : len(value)-1])
		}
	}
	return value
}
