//go:build !(linux && (amd64 || arm64))

package gitjail

const supported = false

type hostSystem struct{ Git string }

func system() (hostSystem, error) { return hostSystem{}, nil }

// ABI reports the Landlock ABI git runs under; 0 means unconfined.
func ABI() int { return 0 }

func (c *Cmd) start() error { return c.Cmd.Start() }
