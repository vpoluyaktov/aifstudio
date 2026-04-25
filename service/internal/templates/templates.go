package templates

import "embed"

// HTMLFS contains all HTML template files embedded at compile time.
//
//go:embed *.html
var HTMLFS embed.FS

// StaticFS contains static assets (CSS, JS) served from /static/.
//
//go:embed static/*
var StaticFS embed.FS
