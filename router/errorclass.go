package router

import (
	"context"
	"errors"

	"github.com/Loomux/server/orchestrator"
	"github.com/Loomux/server/registry"
	"github.com/Loomux/server/targets"
)

// classifyDispatchError maps a dispatch error to a stable error_class
// label for metrics, reusing registry.ErrorClass values from LOOM-77.
// It keeps cardinality bounded by never using the error message as a
// label. The empty string means "no error" (success).
func classifyDispatchError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, targets.ErrUnreachable) {
		return string(registry.ErrorClassTargetUnreachable)
	}
	if errors.Is(err, orchestrator.ErrHumanTakeover) {
		return string(registry.ErrorClassInternal)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return string(registry.ErrorClassWaitFailed)
	}
	return string(registry.ErrorClassInternal)
}
