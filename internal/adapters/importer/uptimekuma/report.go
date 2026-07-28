package uptimekuma

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// SkipReason records one skipped source entity. It must never contain secrets.
type SkipReason struct {
	Kind   string `json:"kind"`           // monitor, notification, proxy, ...
	ID     int64  `json:"id"`             // source-side ID
	Type   string `json:"type,omitempty"` // source type string when known
	Name   string `json:"name,omitempty"` // display name only
	Reason string `json:"reason"`         // human-readable skip cause
}

// Report is a safe conversion summary (counts + reasons, no secrets).
type Report struct {
	GeneratedAt          time.Time    `json:"generated_at"`
	SourcePath           string       `json:"source_path,omitempty"`   // file path or sanitized DSN label
	SourceEngine         string       `json:"source_engine,omitempty"` // sqlite | mariadb
	SchemaVariant        string       `json:"schema_variant,omitempty"`
	Proxies              int          `json:"proxies"`
	Notifications        int          `json:"notifications"`
	Tags                 int          `json:"tags"`
	MonitorGroups        int          `json:"monitor_groups"`
	Monitors             int          `json:"monitors"`
	MonitorTags          int          `json:"monitor_tags"`
	MonitorNotifications int          `json:"monitor_notifications"`
	StatusPages          int          `json:"status_pages"`
	StatusPageMonitors   int          `json:"status_page_monitors"`
	StatusPageCNAMEs     int          `json:"status_page_cnames"`
	MaintenanceWindows   int          `json:"maintenance_windows"`
	MaintenanceMonitors  int          `json:"maintenance_monitors"`
	Skipped              []SkipReason `json:"skipped"`
	SkipCount            int          `json:"skip_count"`
}

// WriteReport writes a JSON report with mode 0600.
func WriteReport(path string, r *Report) error {
	if r == nil {
		return fmt.Errorf("nil report")
	}
	// Deterministic skip ordering for reviewable diffs.
	sort.SliceStable(r.Skipped, func(i, j int) bool {
		a, b := r.Skipped[i], r.Skipped[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Reason < b.Reason
	})
	r.SkipCount = len(r.Skipped)
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = time.Now().UTC()
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	data = append(data, '\n')
	return writeFile0600(path, data)
}

// writeFile0600 creates path with mode 0600 (owner read/write only).
func writeFile0600(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Re-assert mode in case umask widened it.
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return f.Close()
}
