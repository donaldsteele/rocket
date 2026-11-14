// Package web holds the embedded single-page UI assets.
package web

import (
	"embed"
	"io/fs"
	"mime"
)

//go:embed index.html app.js styles.css theme.js icons.svg favicon.png drop.html drop.js vendor/*
var files embed.FS

// Go's built-in MIME table has no WOFF2 entry, so register it for the fonts.
func init() {
	_ = mime.AddExtensionType(".woff2", "font/woff2")
}

// FS returns the static asset tree rooted at the web directory.
func FS() fs.FS { return files }
