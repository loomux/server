package providers

import (
	"context"

	"github.com/Loomux/server/plugins/protocol"
)

// Hooks for providers_test into the job machinery.

var ErrStaleForTest = errStale

func (m *Manager) StartJobForTest(envID string, fn func(ctx context.Context)) { m.startJob(envID, fn) }
func (m *Manager) CancelJobForTest(envID string)                              { m.cancelJob(envID) }
func (m *Manager) FollowingForTest(envID string) bool                         { return m.following(envID) }
func (m *Manager) WaitJobsForTest()                                           { m.jobs.Wait() }

func (m *Manager) ApplyLockedForTest(ctx context.Context, envID string, pe protocol.Environment) error {
	return m.applyLocked(ctx, envID, pe)
}
