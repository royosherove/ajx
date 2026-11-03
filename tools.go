//go:build tools

// Package ajx pins the agreed third-party dependencies of the Go CLI so that
// `go mod vendor` keeps them even before every package imports them.
package ajx

import (
	_ "charm.land/huh/v2"
	_ "charm.land/lipgloss/v2"
	_ "github.com/BurntSushi/toml"
)
