//go:build !linux

package sweep

import (
	"log/slog"
	"time"
)

// Supported reports whether this platform can find escaped descendants.
const Supported = false

// Stop does nothing without /proc: escaped descendants outlive the agent.
func (Marker) Stop(*slog.Logger, time.Duration) {}
