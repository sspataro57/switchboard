//go:build integration

package pipeline_test

// SWT-40 Part E review fix: a clean shutdown publishes the retained dead
// payload deliberately (fleet.Client.PublishDead), because a clean DISCONNECT
// suppresses the LWT. Compose broker only (requirePipelineEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

func TestStageClient_Integration_CleanShutdownPublishesDead(t *testing.T) {
	broker := requirePipelineEnv(t)
	stage := pipeline.StageRoute
	admin := adminClient(t, broker, "itest-pipeline-dead-admin")
	defer admin.Disconnect(250)
	clearStageStatus(t, admin, stage)
	defer clearStageStatus(t, admin, stage)

	c, err := pipeline.DialStage(context.Background(), broker, stage)
	if err != nil {
		t.Fatalf("DialStage: %v", err)
	}
	if err := c.PublishStatus(fleet.Status{State: fleet.StateIdle}); err != nil {
		t.Fatalf("PublishStatus: %v", err)
	}
	if err := c.PublishDead(); err != nil {
		t.Fatalf("PublishDead: %v", err)
	}
	c.Disconnect() // clean: the broker fires no will

	ch, stop := watchStatus(t, broker, "itest-pipeline-dead-sub", stage)
	defer stop()
	m := waitState(t, ch, fleet.StateDead, 5*time.Second)
	if !m.retained {
		t.Errorf("the shutdown dead was not retained; a view connecting later would see the stale idle")
	}
}
