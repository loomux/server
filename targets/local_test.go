package targets_test

import (
	"testing"

	"github.com/Loomux/server/internal/tmuxtest"
	"github.com/Loomux/server/targets"
	"github.com/Loomux/server/targets/executortest"
)

func TestLocalExecutor(t *testing.T) {
	executortest.Run(t, func(t *testing.T) targets.TargetExecutor {
		exec := targets.NewLocalExecutor(tmuxtest.Socket(t))
		t.Cleanup(func() {
			if err := exec.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return exec
	})
}
