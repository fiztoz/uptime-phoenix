package checker

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/google/uuid"
)

// MQTTChecker checks MQTT broker connectivity using eclipse/paho.mqtt.golang.
type MQTTChecker struct{}

func init() { Register(MQTTChecker{}) }

// Type returns the monitor type identifier.
func (MQTTChecker) Type() string { return "mqtt" }

// mqttBrokerURL resolves the broker address from config. Accepts broker (canonical),
// url (older UI key), or hostname/host + optional port for import-shaped configs.
func mqttBrokerURL(config map[string]any) string {
	if broker, _ := config["broker"].(string); strings.TrimSpace(broker) != "" {
		return strings.TrimSpace(broker)
	}
	if u, _ := config["url"].(string); strings.TrimSpace(u) != "" {
		return strings.TrimSpace(u)
	}
	host, _ := config["hostname"].(string)
	if host == "" {
		host, _ = config["host"].(string)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	// Already a full URL (e.g. mqtt://… pasted into hostname by mistake).
	if strings.Contains(host, "://") {
		return host
	}
	port := 1883
	switch p := config["port"].(type) {
	case float64:
		if p > 0 {
			port = int(p)
		}
	case int:
		if p > 0 {
			port = p
		}
	case int64:
		if p > 0 {
			port = int(p)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil && n > 0 {
			port = n
		}
	}
	return fmt.Sprintf("tcp://%s:%d", host, port)
}

// Validate checks that the required config fields are present and valid.
func (MQTTChecker) Validate(config map[string]any) error {
	if mqttBrokerURL(config) == "" {
		return fmt.Errorf("broker is required (broker URL, e.g. mqtt://host:1883 or wss://host:8084/mqtt)")
	}
	return nil
}

// Check performs an MQTT broker connectivity check.
// Config fields:
//   - broker (required, string) — full broker URL; aliases: url, or hostname/host + port
//   - topic (optional, string, default "#") — subscribe filter
//   - success_message (optional, string) — require payload containing this substring
//   - username / password (optional)
//   - timeout (optional, float64 seconds, default 10)
//
// For MQTT over WebSocket, put the path in the URL (e.g. wss://host:8084/mqtt).
// There is no separate websocket_path field.
func (MQTTChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	// Extract config fields.
	broker := mqttBrokerURL(config)
	topic, _ := config["topic"].(string)
	username, _ := config["username"].(string)
	password, _ := config["password"].(string)
	successMessage, _ := config["success_message"].(string)

	if broker == "" {
		return ports.CheckResult{Status: domain.StatusDown, Message: "broker is required"}, nil
	}

	if topic == "" {
		topic = "#"
	}

	// Extract timeout (default 10s).
	timeout := 10.0
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = t
	}

	// Apply timeout via context.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()

	start := time.Now()

	// Create client options.
	clientID := fmt.Sprintf("phoenix-check-%s", uuid.New().String()[:8])
	opts := mqtt.NewClientOptions().
		AddBroker(broker).
		SetClientID(clientID).
		SetAutoReconnect(false).
		SetConnectTimeout(time.Duration(timeout * float64(time.Second))).
		SetCleanSession(true)

	if username != "" {
		opts.SetUsername(username)
	}
	if password != "" {
		opts.SetPassword(password)
	}

	// Channels to signal connection and message receipt.
	connected := make(chan struct{}, 1)
	messageReceived := make(chan struct{}, 1)

	// OnConnect handler: subscribe to topic.
	opts.SetOnConnectHandler(func(client mqtt.Client) {
		token := client.Subscribe(topic, 1, nil)
		token.Wait()
		if token.Error() == nil {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
	})

	// DefaultPublishHandler: check for success_message if configured.
	if successMessage != "" {
		opts.SetDefaultPublishHandler(func(client mqtt.Client, msg mqtt.Message) {
			payload := string(msg.Payload())
			if strings.Contains(payload, successMessage) {
				select {
				case messageReceived <- struct{}{}:
				default:
				}
			}
		})
	}

	// Create and connect client.
	client := mqtt.NewClient(opts)
	token := client.Connect()
	token.Wait()

	if token.Error() != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: time.Since(start).Milliseconds(),
			Message:   fmt.Sprintf("mqtt connect failed: %v", token.Error()),
		}, nil
	}

	// Wait for connection confirmation (subscription completed).
	select {
	case <-connected:
	case <-ctx.Done():
		client.Disconnect(100)
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: time.Since(start).Milliseconds(),
			Message:   fmt.Sprintf("mqtt subscribe timeout: %v", ctx.Err()),
		}, nil
	}

	// If success_message is set, wait for a matching message.
	if successMessage != "" {
		select {
		case <-messageReceived:
			// Message matched - check passed.
			latencyMs := time.Since(start).Milliseconds()
			client.Disconnect(100)
			return ports.CheckResult{
				Status:    domain.StatusUp,
				LatencyMs: latencyMs,
				Message:   fmt.Sprintf("mqtt connected, message received on topic %s", topic),
			}, nil
		case <-ctx.Done():
			client.Disconnect(100)
			return ports.CheckResult{
				Status:    domain.StatusDown,
				LatencyMs: time.Since(start).Milliseconds(),
				Message:   fmt.Sprintf("mqtt message wait timeout: no message matching %q received on topic %s", successMessage, topic),
			}, nil
		}
	}

	// No success_message required - connection alone is enough.
	latencyMs := time.Since(start).Milliseconds()
	client.Disconnect(100)

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   fmt.Sprintf("mqtt connected to %s, subscribed to %s", broker, topic),
	}, nil
}
