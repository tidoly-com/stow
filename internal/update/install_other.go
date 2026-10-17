//go:build !darwin && !windows

package update

import (
	"context"
	"os/exec"
)

// install: on Linux tidoly stow runs as a service, set up with sudo; the admin page shows the command.
func install(context.Context, string) (*exec.Cmd, error) { return nil, ErrByHand }
