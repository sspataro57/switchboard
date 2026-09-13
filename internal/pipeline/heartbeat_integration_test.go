//go:build integration

package pipeline_test

// SWT-40 Part E, criterion E4, integration half (docs/tickets/inquiry-promote_SPEC.md
// E-D4). Build-tagged `integration` AND gated on MQTT_BROKER plus DATABASE_URL
// (the repo's pairing; this file touches no table). Run with:
//
//	MQTT_BROKER=tcp://localhost:1884 \
//	DATABASE_URL=postgres://ops:ops@localhost:5433/ops?sslmode=disable \
//	  go test -tags integration -p 1 -count=1 -run Heartbeat ./internal/pipeline/
//
// Against compose Mosquitto it asserts, per stage:
//   - status on ops/workers/pipeline.{stage}/status is RETAINED (a fresh
//     subscriber gets it) and QoS 1;
//   - `working` while a pass runs, `idle` after;
//   - killing the process (SIGKILL on a re-exec'd helper, the pod-kill case)
//     makes the broker publish the LWT {"state":"dead"}.
//
// The test clears every retained topic it touched, before and after (IK MQTT
// landmine: retained state is global on a broker). It uses real stage names on
// the compose broker only; the guard refuses the production broker.
//
// GREENFIELD NOTE: compile-FAILs until internal/pipeline exists. Additional
// surface imposed here:
//
//	// DialStage connects a stage's own spine client: client id
//	// StageClientID(stage), a RETAINED {"state":"dead"} will on
//	// fleet.StatusTopic(WorkerID(stage)), QoS 1. The returned client publishes the
//	// stage's heartbeat with PublishStatus and satisfies Publisher/Subscriber.
//	func DialStage(ctx context.Context, broker string, stage Stage) (*fleet.Client, error)

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/sspataro57/switchboard/internal/fleet"
	"github.com/sspataro57/switchboard/internal/pipeline"
)

const helperEnv = "SWT40_PIPELINE_STAGE_HELPER"

// adminClient is a plain paho client for clearing retained topics.
func adminClient(t *testing.T, broker, id string) mqtt.Client {
	t.Helper()
	c := mqtt.NewClient(mqtt.NewClientOptions().AddBroker(broker).SetClientID(id).
		SetConnectTimeout(5 * time.Second).SetAutoReconnect(false))
	if tok := c.Connect(); !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("paho connect %s: %v", id, tok.Error())
	}
	return c
}

// clearStageStatus wipes the retained slot for a stage's status topic.
func clearStageStatus(t *testing.T, c mqtt.Client, stage pipeline.Stage) {
	t.Helper()
	tok := c.Publish(fleet.StatusTopic(pipeline.WorkerID(stage)), 1, true, []byte{})
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("clear retained status for %s: %v", stage, tok.Error())
	}
}

type statusMsg struct {
	state    string
	retained bool
	qos      byte
}

// watchStatus subscribes a stage's status topic and streams parsed messages.
func watchStatus(t *testing.T, broker, id string, stage pipeline.Stage) (<-chan statusMsg, func()) {
	t.Helper()
	out := make(chan statusMsg, 64)
	c := adminClient(t, broker, id)
	tok := c.Subscribe(fleet.StatusTopic(pipeline.WorkerID(stage)), 1, func(_ mqtt.Client, m mqtt.Message) {
		if len(m.Payload()) == 0 {
			return // a clear
		}
		s, err := fleet.ParseStatus(m.Payload())
		if err != nil {
			t.Errorf("unparseable status %q: %v", m.Payload(), err)
			return
		}
		out <- statusMsg{state: s.State, retained: m.Retained(), qos: m.Qos()}
	})
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("subscribe status: %v", tok.Error())
	}
	return out, func() { c.Disconnect(250) }
}

func waitState(t *testing.T, ch <-chan statusMsg, want string, within time.Duration) statusMsg {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-ch:
			if m.state == want {
				return m
			}
		case <-deadline:
			t.Fatalf("status never became %q within %v", want, within)
		}
	}
}

// Working during a pass, idle after, retained, QoS 1.
func TestStageHeartbeat_Integration_RetainedWorkingThenIdle(t *testing.T) {
	broker := requirePipelineEnv(t)
	stage := pipeline.StageRouteApply
	admin := adminClient(t, broker, "itest-pipeline-hb-admin")
	defer admin.Disconnect(250)
	clearStageStatus(t, admin, stage)
	defer clearStageStatus(t, admin, stage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := pipeline.DialStage(ctx, broker, stage)
	if err != nil {
		t.Fatalf("DialStage: %v", err)
	}
	defer client.Disconnect()

	var once sync.Once
	inPass := make(chan struct{})
	release := make(chan struct{})
	loop := pipeline.NewStageLoop(pipeline.StageConfig{
		Stage:  stage,
		Limit:  10,
		Status: client,
		Pass: func(pctx context.Context) (int, error) {
			once.Do(func() { close(inPass) })
			select {
			case <-release:
			case <-pctx.Done():
			}
			return 0, nil
		},
	})
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx) }()

	select {
	case <-inPass:
	case <-time.After(10 * time.Second):
		t.Fatalf("the catch-up pass never started")
	}
	// Wait until `working` has been published: heartbeats go out from their own
	// goroutine, so the first one can reach an already-subscribed client LIVE
	// (retained=false). Only then does a FRESH subscriber prove the retained slot.
	ch0, stop0 := watchStatus(t, broker, "itest-pipeline-hb-sub0", stage)
	waitState(t, ch0, fleet.StateWorking, 5*time.Second)
	stop0()
	ch, stop := watchStatus(t, broker, "itest-pipeline-hb-sub1", stage)
	m := waitState(t, ch, fleet.StateWorking, 5*time.Second)
	stop()
	if !m.retained {
		t.Errorf("status arrived to a new subscriber with retained=false; the stage heartbeat must be retained")
	}
	if m.qos != 1 {
		t.Errorf("status delivered at QoS %d, want 1", m.qos)
	}

	close(release)
	ch2, stop2 := watchStatus(t, broker, "itest-pipeline-hb-sub2", stage)
	defer stop2()
	waitState(t, ch2, fleet.StateIdle, 5*time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Run did not return after cancel")
	}
}

// TestStageHeartbeatHelperProcess is NOT a test: it is the re-exec'd child the
// LWT test kills. It skips unless the parent set helperEnv.
func TestStageHeartbeatHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process for TestStageHeartbeat_Integration_LWTDeadOnKill")
	}
	broker := os.Getenv("MQTT_BROKER")
	stage := pipeline.Stage(os.Getenv(helperEnv + "_STAGE"))
	ctx := context.Background()
	client, err := pipeline.DialStage(ctx, broker, stage)
	if err != nil {
		t.Fatalf("helper DialStage: %v", err)
	}
	loop := pipeline.NewStageLoop(pipeline.StageConfig{
		Stage: stage, Limit: 10, Status: client,
		Pass: func(context.Context) (int, error) { return 0, nil },
	})
	_ = loop.Run(ctx) // runs until SIGKILL
}

// Killing the process fires the broker-side LWT {"state":"dead"}, retained.
func TestStageHeartbeat_Integration_LWTDeadOnKill(t *testing.T) {
	broker := requirePipelineEnv(t)
	stage := pipeline.StageInquiryPromote
	admin := adminClient(t, broker, "itest-pipeline-lwt-admin")
	defer admin.Disconnect(250)
	clearStageStatus(t, admin, stage)
	defer clearStageStatus(t, admin, stage)

	ch, stop := watchStatus(t, broker, "itest-pipeline-lwt-sub", stage)
	defer stop()

	cmd := exec.Command(os.Args[0], "-test.run=^TestStageHeartbeatHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"=1", helperEnv+"_STAGE="+string(stage), "MQTT_BROKER="+broker)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	waitState(t, ch, fleet.StateIdle, 15*time.Second)

	// The pod-kill case: no clean DISCONNECT, so the broker must fire the will.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	killed = true
	waitState(t, ch, fleet.StateDead, 30*time.Second)

	// And it is retained: a fresh subscriber sees `dead`.
	ch2, stop2 := watchStatus(t, broker, "itest-pipeline-lwt-sub2", stage)
	defer stop2()
	m := waitState(t, ch2, fleet.StateDead, 5*time.Second)
	if !m.retained {
		t.Errorf("the dead LWT was not retained; a view connecting after the kill would not see it")
	}
}
