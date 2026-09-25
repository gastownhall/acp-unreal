//go:build !linux

// Package reaper is a no-op outside Linux: there is no child subreaper.
package reaper

import (
	"context"
	"log/slog"
)

// Supported reports whether this platform has a subreaper.
const Supported = false

// Enable does nothing outside Linux.
func Enable() error { return nil }

// Run does nothing outside Linux.
func Run(ctx context.Context) { <-ctx.Done() }

// KillDescendants does nothing outside Linux.
func KillDescendants(*slog.Logger) {}
