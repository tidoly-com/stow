package update

import "context"

// updateDir is where the Linux install keeps its update service's files (scripts/stow-linux.sh): ask/, which
// the account the photo server runs as may write in, and state, which only root writes.
const updateDir = "/var/lib/tidoly-stow"

// install: on Linux tidoly stow runs as a service under someone's account, and replacing it takes root. The
// install sets up a root service of its own, tidoly-stow-update, that systemd starts when a request appears
// in ask/; it runs the same install script the customer ran, apart from this program. An install from before
// it has no ask/, and the admin page shows the command instead.
func install(context.Context, string) (func() error, error) {
	if !askable(updateDir) {
		return nil, ErrByHand
	}
	return ask(updateDir)
}

func byButton() bool { return askable(updateDir) }
