// Package web contains the standalone, read-only k8gb-ui frontend.
package web

import "embed"

// Assets includes index.html at its root and all offline static dependencies.
// Serve it directly or use fs.Sub(Assets, "static") for /static/ routes.
//
//go:embed index.html static/*
var Assets embed.FS
