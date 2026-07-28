package repository

import (
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TLSInfoModelFromPort converts a ports.TLSInfo to a TLSInfoModel for persistence.
// Certificate-alert threshold state is stored inside info_json so a worker restart
// does not re-send the same threshold for the same certificate.
func TLSInfoModelFromPort(info *ports.TLSInfo) *TLSInfoModel {
	jsonMap := JSONField{
		"days_remaining": info.DaysRemaining,
		"not_after":      info.NotAfter.UTC().Format(time.RFC3339),
		"issuer":         info.Issuer,
	}
	if info.LastCertAlertThreshold > 0 {
		jsonMap["last_cert_alert_threshold"] = info.LastCertAlertThreshold
	}
	if !info.LastCertAlertNotAfter.IsZero() {
		jsonMap["last_cert_alert_not_after"] = info.LastCertAlertNotAfter.UTC().Format(time.RFC3339)
	}
	return &TLSInfoModel{
		MonitorID: info.MonitorID,
		InfoJSON:  jsonMap,
		CheckedAt: info.CheckedAt.UTC(),
	}
}

// ToPort converts a TLSInfoModel to ports.TLSInfo.
func (m *TLSInfoModel) ToPort() (*ports.TLSInfo, error) {
	info := &ports.TLSInfo{
		MonitorID: m.MonitorID,
		CheckedAt: m.CheckedAt,
	}

	if m.InfoJSON != nil {
		if v, ok := m.InfoJSON["days_remaining"]; ok {
			switch n := v.(type) {
			case float64:
				info.DaysRemaining = int(n)
			case int:
				info.DaysRemaining = n
			case int64:
				info.DaysRemaining = int(n)
			}
		}
		if v, ok := m.InfoJSON["not_after"].(string); ok && v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("parse tls not_after: %w", err)
			}
			info.NotAfter = t
		}
		if v, ok := m.InfoJSON["issuer"].(string); ok {
			info.Issuer = v
		}
		if v, ok := m.InfoJSON["last_cert_alert_threshold"]; ok {
			switch n := v.(type) {
			case float64:
				info.LastCertAlertThreshold = int(n)
			case int:
				info.LastCertAlertThreshold = n
			case int64:
				info.LastCertAlertThreshold = int(n)
			}
		}
		if v, ok := m.InfoJSON["last_cert_alert_not_after"].(string); ok && v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, fmt.Errorf("parse last_cert_alert_not_after: %w", err)
			}
			info.LastCertAlertNotAfter = t
		}
	}

	return info, nil
}
