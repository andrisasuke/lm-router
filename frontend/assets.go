//go:build wailsapp

package frontendassets

import "embed"

// Assets contains the production frontend bundle embedded by the desktop app.
//
//go:embed all:dist
var Assets embed.FS
