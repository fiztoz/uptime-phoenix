package checker

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"

	amqp "github.com/rabbitmq/amqp091-go"
)

const defaultRabbitMQPort = 5672

// RabbitMQChecker checks RabbitMQ AMQP 0-9-1 broker connectivity.
type RabbitMQChecker struct{}

func init() { Register(RabbitMQChecker{}) }

// Type returns the monitor type identifier.
func (RabbitMQChecker) Type() string { return "rabbitmq" }

// rabbitMQURL resolves the AMQP URL from config. Accepts url (canonical),
// connection_string/dsn aliases, or hostname/host + optional port, username,
// password, vhost, and tls fields for form/import-shaped configs.
func rabbitMQURL(config map[string]any) string {
	for _, key := range []string{"url", "connection_string", "dsn"} {
		if raw, _ := config[key].(string); strings.TrimSpace(raw) != "" {
			return strings.TrimSpace(raw)
		}
	}

	hostname, _ := config["hostname"].(string)
	if hostname == "" {
		hostname, _ = config["host"].(string)
	}
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return ""
	}
	if strings.Contains(hostname, "://") {
		return hostname
	}

	port := defaultRabbitMQPort
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

	scheme := "amqp"
	if tlsEnabled(config["tls"]) || port == 5671 {
		scheme = "amqps"
	}

	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(hostname, strconv.Itoa(port))}
	username, _ := config["username"].(string)
	password, _ := config["password"].(string)
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	if vhost, _ := config["vhost"].(string); strings.TrimSpace(vhost) != "" {
		u.Path = "/" + url.PathEscape(strings.TrimSpace(vhost))
	}
	return u.String()
}

func tlsEnabled(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.EqualFold(strings.TrimSpace(v), "yes") || strings.TrimSpace(v) == "1"
	default:
		return false
	}
}

func rabbitMQTimeout(config map[string]any) time.Duration {
	timeout := 10.0
	switch t := config["timeout"].(type) {
	case float64:
		if t > 0 {
			timeout = t
		}
	case int:
		if t > 0 {
			timeout = float64(t)
		}
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil && n > 0 {
			timeout = n
		}
	}
	return time.Duration(timeout * float64(time.Second))
}

func validateRabbitMQURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required (AMQP URL, e.g. amqp://user:pass@host:5672/vhost)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url is invalid: %w", err)
	}
	if u.Scheme != "amqp" && u.Scheme != "amqps" {
		return fmt.Errorf("url scheme must be amqp or amqps")
	}
	if u.Host == "" {
		return fmt.Errorf("url host is required")
	}
	return nil
}

// Validate checks that the required config fields are present and valid.
func (RabbitMQChecker) Validate(config map[string]any) error {
	if err := validateRabbitMQURL(rabbitMQURL(config)); err != nil {
		return err
	}
	if queue, _ := config["queue"].(string); strings.TrimSpace(queue) != "" {
		return nil
	}
	if exchange, _ := config["exchange"].(string); strings.TrimSpace(exchange) != "" {
		return nil
	}
	return nil
}

// Check performs a RabbitMQ AMQP 0-9-1 connectivity check.
// Config fields:
//   - url (required, string) — full amqp:// or amqps:// URL; aliases: connection_string, dsn, or hostname/host + port
//   - queue (optional, string) — if set, passively declare the queue to verify it exists
//   - exchange (optional, string) — if set, passively declare the exchange to verify it exists
//   - exchange_type (optional, string, default "direct") — required by AMQP for exchange passive declare
//   - username / password / vhost / tls (optional, used only with hostname/host form)
//   - timeout (optional, float64 seconds, default 10)
func (RabbitMQChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	amqpURL := rabbitMQURL(config)
	if err := validateRabbitMQURL(amqpURL); err != nil {
		return ports.CheckResult{Status: domain.StatusDown, Message: err.Error()}, nil
	}

	timeout := rabbitMQTimeout(config)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := amqp.DialConfig(amqpURL, amqp.Config{
		Locale:    "en_US",
		Heartbeat: 10 * time.Second,
		Properties: amqp.Table{
			"product": "phoenix",
		},
		Dial: func(network, addr string) (net.Conn, error) {
			c, dialErr := dialer.DialContext(ctx, network, addr)
			if dialErr != nil {
				return nil, dialErr
			}
			_ = c.SetDeadline(time.Now().Add(timeout))
			return c, nil
		},
	})
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("rabbitmq connect failed: %v", err),
		}, nil
	}
	defer func() { _ = conn.Close() }()

	channel, err := conn.Channel()
	latencyMs = time.Since(start).Milliseconds()
	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("rabbitmq channel open failed: %v", err),
		}, nil
	}
	defer func() { _ = channel.Close() }()

	queue, _ := config["queue"].(string)
	queue = strings.TrimSpace(queue)
	if queue != "" {
		if _, err := channel.QueueDeclarePassive(queue, false, false, false, false, nil); err != nil {
			return ports.CheckResult{
				Status:    domain.StatusDown,
				LatencyMs: time.Since(start).Milliseconds(),
				Message:   fmt.Sprintf("rabbitmq queue %q check failed: %v", queue, err),
			}, nil
		}
	}

	exchange, _ := config["exchange"].(string)
	exchange = strings.TrimSpace(exchange)
	if exchange != "" {
		exchangeType, _ := config["exchange_type"].(string)
		if strings.TrimSpace(exchangeType) == "" {
			exchangeType = "direct"
		}
		if err := channel.ExchangeDeclarePassive(exchange, exchangeType, false, false, false, false, nil); err != nil {
			return ports.CheckResult{
				Status:    domain.StatusDown,
				LatencyMs: time.Since(start).Milliseconds(),
				Message:   fmt.Sprintf("rabbitmq exchange %q check failed: %v", exchange, err),
			}, nil
		}
	}

	message := "rabbitmq connected and opened channel"
	if queue != "" {
		message = fmt.Sprintf("rabbitmq connected and queue %q exists", queue)
	} else if exchange != "" {
		message = fmt.Sprintf("rabbitmq connected and exchange %q exists", exchange)
	}

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: time.Since(start).Milliseconds(),
		Message:   message,
	}, nil
}
