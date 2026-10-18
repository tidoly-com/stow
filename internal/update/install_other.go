//go:build !darwin && !windows && !linux

package update

import "context"

// install: elsewhere the admin page shows how to install by hand.
func install(context.Context, string) (func() error, error) { return nil, ErrByHand }

func byButton() bool { return false }
