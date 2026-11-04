package update

import "context"

// New refuses to run on Windows, which does not let a running binary be
// replaced by a rename. These stubs keep the package building there.

func (u *Updater) writable() error { return errUnsupported }

func (u *Updater) install(context.Context, []byte, semver) error { return errUnsupported }
