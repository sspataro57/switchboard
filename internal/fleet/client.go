package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const qos = 1

// Client is a thin wrapper over paho. Two modes:
//   - worker mode (NewWorkerClient): the retained {"state":"dead"} LWT is
//     mandatory — a worker cannot connect without one.
//   - mirror mode (NewMirrorClient): no will; used by fleetd.
//
// Subscriptions are re-established in the OnConnect handler so paho's
// auto-reconnect survives broker restarts (clean sessions drop them).
type Client struct {
	c        mqtt.Client
	workerID string // set in worker mode

	mu   sync.Mutex
	subs map[string]mqtt.MessageHandler
}

func newClient(ctx context.Context, brokerURL, clientID string, will bool, workerID string) (*Client, error) {
	cl := &Client{workerID: workerID, subs: map[string]mqtt.MessageHandler{}}

	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID(clientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetKeepAlive(30 * time.Second).
		SetConnectTimeout(10 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			cl.mu.Lock()
			defer cl.mu.Unlock()
			for filter, h := range cl.subs {
				filter := filter
				tok := c.Subscribe(filter, qos, h)
				go func() {
					tok.Wait()
					if err := tok.Error(); err != nil {
						slog.Error("resubscribe failed after reconnect", "filter", filter, "err", err)
					}
				}()
			}
		})
	if will {
		opts.SetBinaryWill(StatusTopic(workerID), LWTPayload(), qos, true)
	}

	cl.c = mqtt.NewClient(opts)
	tok := cl.c.Connect()
	select {
	case <-tok.Done():
	case <-ctx.Done():
		return nil, fmt.Errorf("connect %s: %w", brokerURL, ctx.Err())
	}
	if err := tok.Error(); err != nil {
		return nil, fmt.Errorf("connect %s: %w", brokerURL, err)
	}
	return cl, nil
}

// NewWorkerClient connects a worker: the retained dead LWT on the worker's own
// status topic is always registered (invariant of the contract, criterion 4).
func NewWorkerClient(ctx context.Context, brokerURL, workerID string) (*Client, error) {
	if err := ValidateWorkerID(workerID); err != nil {
		return nil, fmt.Errorf("worker client: %w", err)
	}
	return newClient(ctx, brokerURL, "switchboard-worker-"+workerID, true, workerID)
}

// NewMirrorClient connects fleetd: no will, stable client id (a second
// instance takes over rather than split-braining the table).
func NewMirrorClient(ctx context.Context, brokerURL string) (*Client, error) {
	return newClient(ctx, brokerURL, "switchboard-fleetd", false, "")
}

// PublishStatus publishes this worker's heartbeat — retained, QoS 1, strict
// vocabulary. Worker mode only.
func (c *Client) PublishStatus(s Status) error {
	if c.workerID == "" {
		return fmt.Errorf("PublishStatus requires a worker-mode client")
	}
	payload, err := s.Marshal()
	if err != nil {
		return fmt.Errorf("publish status: %w", err)
	}
	tok := c.c.Publish(StatusTopic(c.workerID), qos, true, payload)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("publish status: %w", err)
	}
	return nil
}

// PublishCommand publishes a command to a worker — NOT retained (a retained
// cmd would re-fire on every reconnect), QoS 1.
func (c *Client) PublishCommand(workerID string, cmd Cmd) error {
	if err := ValidateWorkerID(workerID); err != nil {
		return fmt.Errorf("publish command: %w", err)
	}
	payload, err := cmd.Marshal()
	if err != nil {
		return fmt.Errorf("publish command: %w", err)
	}
	tok := c.c.Publish(CmdTopic(workerID), qos, false, payload)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("publish command to %s: %w", workerID, err)
	}
	return nil
}

// SubscribeStatus subscribes ops/workers/+/status and registers the handler
// for OnConnect re-subscription.
func (c *Client) SubscribeStatus(handler func(topic string, payload []byte)) error {
	h := func(_ mqtt.Client, msg mqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	}
	c.mu.Lock()
	c.subs[StatusFilter] = h
	c.mu.Unlock()

	tok := c.c.Subscribe(StatusFilter, qos, h)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("subscribe %s: %w", StatusFilter, err)
	}
	return nil
}

// Disconnect closes cleanly (suppressing any LWT).
func (c *Client) Disconnect() {
	c.c.Disconnect(250)
}
