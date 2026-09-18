// Package web embeds the frontend (templates + static assets) into the
// compiled binary. The production Dockerfile stages a distroless image that
// contains only the server binary, so every file the process needs at
// runtime — templates, CSS, and JS — must live inside it. This mirrors the
// approach used for migrations/.
package web

import "embed"

//go:embed templates static
var FS embed.FS
