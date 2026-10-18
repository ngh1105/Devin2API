// Package web embeds the static assets for the admin panel.
package web

import "embed"

//go:embed accounts.html tokens.html settings.html app.js styles.css

// FS contains all embedded web assets.
var FS embed.FS
