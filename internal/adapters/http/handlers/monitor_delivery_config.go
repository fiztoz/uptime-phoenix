package handlers

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type monitorDeliveryConfigReader interface {
	AcknowledgedDeliveryConfigs(context.Context, int64, int64, int64, time.Time) (map[string]services.DeliveryConfigEvidence, error)
}

// DeliveryConfigEvidenceView exposes document acknowledgement only. Counters are
// decimal strings; a desired document mode never implies runtime application.
type DeliveryConfigEvidenceView struct {
	State          string  `json:"state"`
	ConfigRevision *string `json:"config_revision"`
	Generation     *string `json:"generation"`
	AlertDelivery  *string `json:"alert_delivery"`
}

func deliveryConfigQuery(raw string) (bool, error) {
	// This route historically ignored all query parameters. Decode only the new
	// key/value; malformed unrelated parameters must not change default GETs.
	found, include := false, false
	for _, pair := range strings.Split(raw, "&") {
		key, value, _ := strings.Cut(pair, "=")
		key, err := url.QueryUnescape(key)
		if err != nil || key != "include_delivery_config" {
			continue
		}
		value, err = url.QueryUnescape(value)
		if err != nil || found || value != "true" && value != "false" {
			return false, errors.New("invalid delivery configuration query")
		}
		found, include = true, value == "true"
	}
	return include, nil
}

func toDeliveryConfigEvidenceView(e services.DeliveryConfigEvidence) *DeliveryConfigEvidenceView {
	return &DeliveryConfigEvidenceView{State: e.State, ConfigRevision: fleetRevision(e.ConfigRevision), Generation: fleetRevision(e.Generation), AlertDelivery: optionalString(string(e.AlertDelivery))}
}
