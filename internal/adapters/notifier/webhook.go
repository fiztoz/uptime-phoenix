package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"text/template"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// WebhookSender implements NotificationSender for generic webhooks.
type WebhookSender struct{}

func init() { Register(WebhookSender{}) }

func (WebhookSender) Type() string { return "webhook" }

func (WebhookSender) Validate(config map[string]any) error {
	if v, ok := config["url"].(string); !ok || v == "" {
		return fmt.Errorf("url is required")
	}
	return nil
}

func (WebhookSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	url, _ := config["url"].(string)
	method := http.MethodPost
	if m, ok := config["method"].(string); ok && m != "" {
		method = m
	}
	headers := map[string]string{}
	if h, ok := config["headers"].(map[string]any); ok {
		for k, v := range h {
			if s, ok := v.(string); ok {
				headers[k] = s
			}
		}
	}

	var payload []byte
	_, customBody, custom, err := renderCustomLayout(alert)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	if custom {
		payload = []byte(customBody)
		if _, exists := headers["Content-Type"]; !exists && json.Valid(payload) {
			headers["Content-Type"] = "application/json"
		}
	} else if tmplStr, ok := config["body_template"].(string); ok && tmplStr != "" {
		tmpl, err := template.New("webhook").Parse(tmplStr)
		if err != nil {
			return fmt.Errorf("webhook: invalid body_template: %w", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, alert); err != nil {
			return fmt.Errorf("webhook: executing template: %w", err)
		}
		payload = buf.Bytes()
	} else {
		// default JSON body — includes event_kind and cert fields when applicable
		payload, _ = json.Marshal(webhookEventPayload(alert))
		headers["Content-Type"] = "application/json"
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, method, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		// basic auth support via headers already
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("webhook: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("webhook: error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
