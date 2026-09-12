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
		// Stop the attempt: a CONNACK arriving after we gave up would otherwise
		// leave an unowned client auto-reconnecting forever under this id,
		// kicking every later client that uses it off the broker.
		cl.c.Disconnect(0)
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

// NewSpineClient connects a spine service (orchestratord etc.) as a command
// publisher: no will, caller-chosen client id. Do NOT reuse NewMirrorClient —
// its hardcoded id would kick fleetd off the broker (same-client-id takeover).
func NewSpineClient(ctx context.Context, brokerURL, clientID string) (*Client, error) {
	if clientID == "" {
		return nil, fmt.Errorf("spine client requires a distinct client id")
	}
	return newClient(ctx, brokerURL, clientID, false, "")
}

// NewWillClient connects a spine participant that owns a heartbeat (SWT-40
// pipelined stages): a caller-chosen client id AND the retained dead LWT on
// workerID's status topic — NewWorkerClient's will without its fixed
// switchboard-worker- id prefix. The client id must be distinct per
// connection (same-client-id takeover).
func NewWillClient(ctx context.Context, brokerURL, clientID, workerID string) (*Client, error) {
	if clientID == "" {
		return nil, fmt.Errorf("will client requires a distinct client id")
	}
	if err := ValidateWorkerID(workerID); err != nil {
		return nil, fmt.Errorf("will client: %w", err)
	}
	return newClient(ctx, brokerURL, clientID, true, workerID)
}

// publishTimeout bounds the generic Publish's wait for its ack, so a caller on
// a dead or reconnecting broker gets an error instead of a hang.
const publishTimeout = 10 * time.Second

// Publish is the generic publish, QoS and retain chosen by the caller, who owns
// the topic's contract (SWT-40: pipeline wake-ups are QoS 1 and NEVER retained,
// enforced by internal/pipeline's structure test).
func (c *Client) Publish(topic string, qos byte, retained bool, payload []byte) error {
	tok := c.c.Publish(topic, qos, retained, payload)
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish %s: no ack within %v", topic, publishTimeout)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("publish %s: %w", topic, err)
	}
	return nil
}

// Subscribe subscribes filter at QoS 1 and registers the handler for OnConnect
// re-subscription, like SubscribeStatus.
func (c *Client) Subscribe(filter string, handler func(topic string, payload []byte)) error {
	h := func(_ mqtt.Client, msg mqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	}
	c.mu.Lock()
	c.subs[filter] = h
	c.mu.Unlock()

	tok := c.c.Subscribe(filter, qos, h)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("subscribe %s: %w", filter, err)
	}
	return nil
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
	// Bounded: paho holds a QoS 1 publish while it reconnects, and a caller
	// blocked here forever would stop heartbeating anything at all.
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish status: no ack within %v", publishTimeout)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("publish status: %w", err)
	}
	return nil
}

// PublishDead publishes the retained dead payload on this client's own status
// topic, the same bytes the broker publishes as its LWT. A clean DISCONNECT
// suppresses the will, so a service that stops cleanly calls this first:
// dead then means "not running" whether the process crashed or was stopped.
// Status.Marshal still refuses dead; this is the one deliberate path. Worker
// (will-carrying) clients only.
func (c *Client) PublishDead() error {
	if c.workerID == "" {
		return fmt.Errorf("PublishDead requires a worker-mode client")
	}
	tok := c.c.Publish(StatusTopic(c.workerID), qos, true, LWTPayload())
	if !tok.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish dead: no ack within %v", publishTimeout)
	}
	if err := tok.Error(); err != nil {
		return fmt.Errorf("publish dead: %w", err)
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

// SubscribeCmd subscribes this worker's own cmd topic (worker mode only).
// Malformed payloads are logged and dropped — commands are lenient-consume
// like status.
func (c *Client) SubscribeCmd(handler func(Cmd)) error {
	if c.workerID == "" {
		return fmt.Errorf("SubscribeCmd requires a worker-mode client")
	}
	topic := CmdTopic(c.workerID)
	h := func(_ mqtt.Client, msg mqtt.Message) {
		cmd, err := ParseCmd(msg.Payload())
		if err != nil {
			slog.Warn("dropping malformed cmd", "topic", msg.Topic(), "err", err)
			return
		}
		handler(cmd)
	}
	c.mu.Lock()
	c.subs[topic] = h
	c.mu.Unlock()

	tok := c.c.Subscribe(topic, qos, h)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("subscribe %s: %w", topic, err)
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
