//go:build !linux

package operate

import (
	"context"
	"errors"
)

func runOperatorShellSandbox(context.Context, string, string) (string, bool, error) {
	return "", false, errors.New("operate: operator shell sandbox requires Linux bubblewrap")
}
