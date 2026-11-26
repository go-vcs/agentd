//go:build !linux && !darwin

package sock

// No peer-credential check is implemented here, so peerUID stays nil and
// Listen refuses: a daemon that cannot tell who is calling does not serve.

func checkDir(string) error { return nil }
