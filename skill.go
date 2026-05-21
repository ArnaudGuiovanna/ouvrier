package ovr

import (
	"fmt"
	"strings"
)

type skillSpec struct {
	dirName string
}

type skillPipeOption struct {
	spec skillSpec
}

// Skill registers a Markdown skill directory for a Pipe.
func Skill(dirName string) PipeOption {
	return skillPipeOption{
		spec: skillSpec{dirName: strings.TrimSpace(dirName)},
	}
}

func (o skillPipeOption) applyPipe(config *pipeConfig) {
	config.skills = append(config.skills, o.spec)
}

func (s skillSpec) validateSkill() error {
	switch {
	case s.dirName == "":
		return fmt.Errorf("%w: Skill directory name is required", ErrInvalidNode)
	case s.dirName == "." || s.dirName == "..":
		return fmt.Errorf("%w: Skill directory name must be a direct child of skills", ErrInvalidNode)
	case strings.ContainsAny(s.dirName, `/\`):
		return fmt.Errorf("%w: Skill directory name must not contain path separators", ErrInvalidNode)
	default:
		return nil
	}
}
