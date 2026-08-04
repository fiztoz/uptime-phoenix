package uptimekuma

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type closeErrer interface {
	Close() error
}

func closeIgnoringError(c closeErrer) {
	_ = c.Close()
}

// Options controls conversion behavior.
type Options struct {
	// Engine selects the source database: "sqlite" (default) or "mariadb"
	// (MySQL is accepted as an alias — same driver and Kuma schema).
	Engine string
	// Input is the path to a Kuma SQLite database file (required for sqlite).
	Input string
	// DSN is a go-sql-driver/mysql connection string (required for mariadb).
	// Example: user:pass@tcp(127.0.0.1:3306)/kuma?parseTime=true
	// Prefer a read-only database user. Passwords must never be logged.
	DSN string
	// Output is the path for the Phoenix backup JSON (required).
	Output string
	// ReportPath, when non-empty, writes a safe summary JSON (no secrets).
	ReportPath string
	// Force overwrites Output if it already exists.
	Force bool
	// Strict causes Convert to return an error when any supported-looking
	// entity is skipped (unsupported type, missing required field, etc.).
	Strict bool
}

// Result holds the conversion document and report.
type Result struct {
	Document *services.BackupDocument
	Report   *Report
}

// Convert opens the Kuma source read-only (SQLite file or MariaDB DSN), builds
// a BackupDocument, and writes it to Options.Output with mode 0600.
func Convert(opts Options) (*Result, error) {
	if strings.TrimSpace(opts.Output) == "" {
		return nil, fmt.Errorf("--output is required")
	}
	eng, err := normalizeEngine(opts.Engine)
	if err != nil {
		return nil, err
	}
	// Default engine is sqlite when --input is set without --engine.
	if strings.TrimSpace(opts.Engine) == "" && strings.TrimSpace(opts.DSN) != "" && strings.TrimSpace(opts.Input) == "" {
		eng = EngineMariaDB
	}
	if eng == EngineSQLite && strings.TrimSpace(opts.Input) == "" {
		return nil, fmt.Errorf("--input is required for engine=sqlite")
	}
	if eng == EngineMariaDB && strings.TrimSpace(opts.DSN) == "" {
		return nil, fmt.Errorf("--dsn is required for engine=mariadb")
	}
	if !opts.Force {
		if _, err := os.Stat(opts.Output); err == nil {
			return nil, fmt.Errorf("output %s already exists (pass --force to overwrite)", opts.Output)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat output: %w", err)
		}
	}

	src, err := openSource(eng, opts.Input, opts.DSN)
	if err != nil {
		return nil, err
	}
	defer closeIgnoringError(src)

	doc, report, err := convertDB(src)
	if err != nil {
		return nil, err
	}

	// Deterministic ordering of every slice for reviewable fixtures.
	sortDocument(doc)
	sort.SliceStable(report.Skipped, func(i, j int) bool {
		a, b := report.Skipped[i], report.Skipped[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Reason < b.Reason
	})
	report.SkipCount = len(report.Skipped)

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal backup document: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(opts.Output), 0o700); err != nil && !os.IsExist(err) {
		// Dir may be "." — MkdirAll(".") is fine; only fail hard otherwise.
		if filepath.Dir(opts.Output) != "." {
			return nil, fmt.Errorf("create output dir: %w", err)
		}
	}
	if err := writeFile0600(opts.Output, data); err != nil {
		return nil, err
	}

	if opts.ReportPath != "" {
		if err := WriteReport(opts.ReportPath, report); err != nil {
			return nil, err
		}
	}

	if opts.Strict && len(report.Skipped) > 0 {
		return &Result{Document: doc, Report: report}, fmt.Errorf(
			"strict mode: %d supported-looking entit(y/ies) skipped — see --report",
			len(report.Skipped),
		)
	}
	return &Result{Document: doc, Report: report}, nil
}

func convertDB(src *source) (*services.BackupDocument, *Report, error) {
	db := src.db
	report := &Report{
		GeneratedAt:  time.Now().UTC(),
		SourcePath:   src.label,
		SourceEngine: src.engine,
		Skipped:      []SkipReason{},
	}
	doc := &services.BackupDocument{
		Version:              services.BackupDocumentVersion,
		ExportedAt:           time.Now().UTC(),
		Proxies:              []services.BackupProxy{},
		Notifications:        []services.BackupNotification{},
		Tags:                 []services.BackupTag{},
		MonitorGroups:        []services.BackupMonitorGroup{},
		Monitors:             []services.BackupMonitor{},
		MonitorTags:          []services.BackupMonitorTag{},
		MonitorNotifications: []services.BackupMonitorNotification{},
		GroupNotifications:   []services.BackupGroupNotification{},
		StatusPages:          []services.BackupStatusPage{},
		StatusPageMonitors:   []services.BackupStatusPageMonitor{},
		StatusPageCNAMEs:     []services.BackupStatusPageCNAME{},
		Incidents:            []services.BackupIncident{}, // deliberately empty — skipped
		MaintenanceWindows:   []services.BackupMaintenance{},
		MaintenanceMonitors:  []services.BackupMaintenanceMonitor{},
	}

	// Detect schema variant from optional columns.
	monitorCols, err := src.tableColumns("monitor")
	if err != nil {
		return nil, nil, fmt.Errorf("inspect monitor table: %w", err)
	}
	if len(monitorCols) == 0 {
		return nil, nil, fmt.Errorf("kuma database missing monitor table")
	}
	if hasCol(monitorCols, "parent") {
		report.SchemaVariant = "with-parent"
	} else {
		report.SchemaVariant = "classic"
	}

	// ── Proxies ──────────────────────────────────────────────────────────
	if ok, _ := src.tableExists("proxy"); ok {
		if err := convertProxies(src, doc, report); err != nil {
			return nil, nil, err
		}
	}

	// ── Tags ─────────────────────────────────────────────────────────────
	if ok, _ := src.tableExists("tag"); ok {
		if err := convertTags(db, doc, report); err != nil {
			return nil, nil, err
		}
	}

	// ── Notifications ────────────────────────────────────────────────────
	notifIDs := map[int64]struct{}{}
	if ok, _ := src.tableExists("notification"); ok {
		var err error
		notifIDs, err = convertNotifications(db, doc, report)
		if err != nil {
			return nil, nil, err
		}
	}

	// ── Monitors + groups (folders are type=group) ───────────────────────
	groupIDs := map[int64]struct{}{}
	monitorIDs := map[int64]struct{}{}
	if err := convertMonitorsAndGroups(src, monitorCols, doc, report, groupIDs, monitorIDs); err != nil {
		return nil, nil, err
	}

	// ── Monitor ↔ tag links ──────────────────────────────────────────────
	if ok, _ := src.tableExists("monitor_tag"); ok {
		if err := convertMonitorTags(db, doc, report, monitorIDs); err != nil {
			return nil, nil, err
		}
	}

	// ── Monitor ↔ notification links ─────────────────────────────────────
	if ok, _ := src.tableExists("monitor_notification"); ok {
		if err := convertMonitorNotifications(db, doc, report, monitorIDs, notifIDs); err != nil {
			return nil, nil, err
		}
	}

	// ── Status pages + order + CNAMEs ────────────────────────────────────
	if ok, _ := src.tableExists("status_page"); ok {
		if err := convertStatusPages(src, doc, monitorIDs); err != nil {
			return nil, nil, err
		}
	}

	// ── Maintenance (optional, when mappable) ────────────────────────────
	if ok, _ := src.tableExists("maintenance"); ok {
		if err := convertMaintenance(src, doc, report, monitorIDs); err != nil {
			return nil, nil, err
		}
	}

	// Refresh counts on the report.
	report.Proxies = len(doc.Proxies)
	report.Notifications = len(doc.Notifications)
	report.Tags = len(doc.Tags)
	report.MonitorGroups = len(doc.MonitorGroups)
	report.Monitors = len(doc.Monitors)
	report.MonitorTags = len(doc.MonitorTags)
	report.MonitorNotifications = len(doc.MonitorNotifications)
	report.StatusPages = len(doc.StatusPages)
	report.StatusPageMonitors = len(doc.StatusPageMonitors)
	report.StatusPageCNAMEs = len(doc.StatusPageCNAMEs)
	report.MaintenanceWindows = len(doc.MaintenanceWindows)
	report.MaintenanceMonitors = len(doc.MaintenanceMonitors)

	return doc, report, nil
}

func convertProxies(src *source, doc *services.BackupDocument, _ *Report) error {
	db := src.db
	// "default" is reserved on MariaDB/MySQL — quote per dialect.
	// SQLite also accepts double-quoted identifiers; MariaDB needs backticks.
	defaultCol := src.quote("default")
	rows, err := db.Query(fmt.Sprintf(`
		SELECT id, protocol, host, port,
		       COALESCE(auth, 0), COALESCE(username, ''), COALESCE(password, ''),
		       COALESCE(active, 1), COALESCE(%s, 0)
		FROM proxy
		ORDER BY id ASC`, defaultCol))
	if err != nil {
		// Older schemas may not have "default" — try without.
		rows, err = db.Query(`
			SELECT id, protocol, host, port,
			       COALESCE(auth, 0), COALESCE(username, ''), COALESCE(password, ''),
			       COALESCE(active, 1), 0
			FROM proxy
			ORDER BY id ASC`)
		if err != nil {
			return fmt.Errorf("query proxy: %w", err)
		}
	}
	defer closeIgnoringError(rows)

	for rows.Next() {
		var p services.BackupProxy
		var auth, active, isDefault int
		if err := rows.Scan(&p.ID, &p.Protocol, &p.Host, &p.Port, &auth, &p.Username, &p.Password, &active, &isDefault); err != nil {
			return fmt.Errorf("scan proxy: %w", err)
		}
		p.Auth = auth != 0
		p.Active = active != 0
		p.IsDefault = isDefault != 0
		doc.Proxies = append(doc.Proxies, p)
	}
	return rows.Err()
}

func convertTags(db *sql.DB, doc *services.BackupDocument, _ *Report) error {
	rows, err := db.Query(`SELECT id, name, COALESCE(color, '#666666') FROM tag ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("query tag: %w", err)
	}
	defer closeIgnoringError(rows)
	for rows.Next() {
		var t services.BackupTag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color); err != nil {
			return fmt.Errorf("scan tag: %w", err)
		}
		doc.Tags = append(doc.Tags, t)
	}
	return rows.Err()
}

func convertNotifications(db *sql.DB, doc *services.BackupDocument, report *Report) (map[int64]struct{}, error) {
	kept := map[int64]struct{}{}
	rows, err := db.Query(`
		SELECT id, COALESCE(name, ''), COALESCE(active, 1), COALESCE(is_default, 0), COALESCE(config, '')
		FROM notification
		ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("query notification: %w", err)
	}
	defer closeIgnoringError(rows)

	for rows.Next() {
		var id int64
		var name, configText string
		var active, isDefault int
		if err := rows.Scan(&id, &name, &active, &isDefault, &configText); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		kumaType, rawCfg, err := parseNotificationConfig(configText)
		if err != nil {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "notification", ID: id, Name: name, Reason: err.Error(),
			})
			continue
		}
		phoenixType, reason := mapNotificationType(kumaType)
		if phoenixType == "" {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "notification", ID: id, Type: kumaType, Name: name, Reason: reason,
			})
			continue
		}
		cfg := convertNotificationConfig(phoenixType, rawCfg)
		doc.Notifications = append(doc.Notifications, services.BackupNotification{
			ID:        id,
			Name:      name,
			Type:      phoenixType,
			Active:    active != 0,
			IsDefault: isDefault != 0,
			Config:    cfg,
		})
		kept[id] = struct{}{}
	}
	return kept, rows.Err()
}

// buildMonitorSelectCols builds the monitor SELECT list and flags for optional
// columns. quote must be the engine dialect's identifier quoter: MariaDB
// reserves INTERVAL (and others), so COALESCE(interval, 60) is a syntax error
// unless the column is backtick-quoted. SQLite double-quotes are fine too.
func buildMonitorSelectCols(
	quote func(string) string,
	cols map[string]struct{},
) (
	selectCols []string,
	hasDescription, hasTimeout, hasParent, hasExpiry, hasJSONPath, hasExpected, hasSystemService, hasSNMPOid, hasSNMPVer bool,
) {
	// interval is reserved on MariaDB/MySQL (INTERVAL expr unit).
	intervalCol := quote("interval")
	selectCols = []string{
		"id",
		"COALESCE(name, '')",
		"COALESCE(type, '')",
		"COALESCE(active, 1)",
		"COALESCE(" + intervalCol + ", 60)",
		"COALESCE(retry_interval, 0)",
		"COALESCE(maxretries, 0)",
		"COALESCE(url, '')",
		"COALESCE(hostname, '')",
		"port",
		"COALESCE(keyword, '')",
		"COALESCE(ignore_tls, 0)",
		"COALESCE(upside_down, 0)",
		"COALESCE(maxredirects, 10)",
		"COALESCE(accepted_statuscodes_json, '[\"200-299\"]')",
		"COALESCE(dns_resolve_type, '')",
		"COALESCE(dns_resolve_server, '')",
		"COALESCE(push_token, '')",
		"COALESCE(method, 'GET')",
		"COALESCE(body, '')",
		"COALESCE(headers, '')",
		"COALESCE(docker_container, '')",
		"proxy_id",
		"COALESCE(mqtt_topic, '')",
		"COALESCE(mqtt_success_message, '')",
		"COALESCE(mqtt_username, '')",
		"COALESCE(mqtt_password, '')",
		"COALESCE(database_connection_string, '')",
		"COALESCE(database_query, '')",
		"COALESCE(grpc_url, '')",
		"COALESCE(grpc_service_name, '')",
		"COALESCE(grpc_enable_tls, 0)",
		"COALESCE(resend_interval, 0)",
		"COALESCE(game, '')",
		"COALESCE(weight, 2000)",
	}

	hasDescription = hasCol(cols, "description")
	hasTimeout = hasCol(cols, "timeout")
	hasParent = hasCol(cols, "parent")
	hasExpiry = hasCol(cols, "expiry_notification")
	hasJSONPath = hasCol(cols, "json_path")
	hasExpected = hasCol(cols, "expected_value")
	hasSystemService = hasCol(cols, "system_service_name")
	hasSNMPOid = hasCol(cols, "snmp_oid") || hasCol(cols, "snmpOid")
	hasSNMPVer = hasCol(cols, "snmp_version") || hasCol(cols, "snmpVersion")

	if hasDescription {
		selectCols = append(selectCols, "COALESCE(description, '')")
	}
	if hasTimeout {
		selectCols = append(selectCols, "COALESCE(timeout, 0)")
	}
	if hasParent {
		selectCols = append(selectCols, "parent")
	}
	if hasExpiry {
		selectCols = append(selectCols, "COALESCE(expiry_notification, 0)")
	}
	if hasJSONPath {
		selectCols = append(selectCols, "COALESCE(json_path, '')")
	}
	if hasExpected {
		selectCols = append(selectCols, "COALESCE(expected_value, '')")
	}
	if hasSystemService {
		selectCols = append(selectCols, "COALESCE(system_service_name, '')")
	}
	if hasCol(cols, "snmp_oid") {
		selectCols = append(selectCols, "COALESCE(snmp_oid, '')")
	} else if hasCol(cols, "snmpOid") {
		selectCols = append(selectCols, "COALESCE(snmpOid, '')")
	}
	if hasCol(cols, "snmp_version") {
		selectCols = append(selectCols, "COALESCE(snmp_version, '')")
	} else if hasCol(cols, "snmpVersion") {
		selectCols = append(selectCols, "COALESCE(snmpVersion, '')")
	}
	return selectCols, hasDescription, hasTimeout, hasParent, hasExpiry, hasJSONPath, hasExpected, hasSystemService, hasSNMPOid, hasSNMPVer
}

func convertMonitorsAndGroups(
	src *source,
	cols map[string]struct{},
	doc *services.BackupDocument,
	report *Report,
	groupIDs map[int64]struct{},
	monitorIDs map[int64]struct{},
) error {
	db := src.db
	selectCols, hasDescription, hasTimeout, hasParent, hasExpiry, hasJSONPath, hasExpected, hasSystemService, hasSNMPOid, hasSNMPVer :=
		buildMonitorSelectCols(src.quote, cols)

	q := `SELECT ` + strings.Join(selectCols, ", ") + ` FROM monitor ORDER BY id ASC`
	rows, err := db.Query(q)
	if err != nil {
		return fmt.Errorf("query monitor: %w", err)
	}
	defer closeIgnoringError(rows)

	var raw []*kumaMonitor
	for rows.Next() {
		m := &kumaMonitor{}
		// Build scan destinations matching selectCols order (fixed first).
		dest := []any{
			&m.ID, &m.Name, &m.Type, boolDest(&m.Active), &m.Interval, &m.RetryInterval, &m.MaxRetries,
			&m.URL, &m.Hostname, nullIntDest(&m.Port), &m.Keyword,
			boolDest(&m.IgnoreTLS), boolDest(&m.UpsideDown), &m.MaxRedirects,
			&m.AcceptedStatuscodesJSON, &m.DNSResolveType, &m.DNSResolveServer,
			&m.PushToken, &m.Method, &m.Body, &m.Headers, &m.DockerContainer,
			nullIntDest(&m.ProxyID),
			&m.MQTTTopic, &m.MQTTSuccessMessage, &m.MQTTUsername, &m.MQTTPassword,
			&m.DatabaseConnectionString, &m.DatabaseQuery,
			&m.GRPCURL, &m.GRPCServiceName, boolDest(&m.GRPCEnableTLS),
			&m.ResendInterval, &m.Game, &m.Weight,
		}
		if hasDescription {
			dest = append(dest, &m.Description)
		}
		if hasTimeout {
			dest = append(dest, &m.Timeout)
		}
		if hasParent {
			dest = append(dest, nullIntDest(&m.Parent))
		}
		if hasExpiry {
			dest = append(dest, boolDest(&m.ExpiryNotification))
		}
		if hasJSONPath {
			dest = append(dest, &m.JSONPath)
		}
		if hasExpected {
			dest = append(dest, &m.ExpectedValue)
		}
		if hasSystemService {
			dest = append(dest, &m.SystemServiceName)
		}
		if hasSNMPOid {
			dest = append(dest, &m.SNMPOid)
		}
		if hasSNMPVer {
			dest = append(dest, &m.SNMPVersion)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan monitor: %w", err)
		}
		raw = append(raw, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// First pass: folders (type=group) → BackupMonitorGroup.
	// Source IDs preserved so parent/monitor links stay coherent.
	for _, m := range raw {
		if strings.ToLower(m.Type) != "group" {
			continue
		}
		g := services.BackupMonitorGroup{
			ID:                 m.ID,
			Name:               m.Name,
			Description:        m.Description,
			Condition:          domain.GroupConditionWorstOfChildren,
			Threshold:          0,
			ThresholdIsPercent: false,
			Weight:             m.Weight,
			Collapsed:          false,
		}
		// Parent link resolved in second pass once all groups are known.
		doc.MonitorGroups = append(doc.MonitorGroups, g)
		groupIDs[m.ID] = struct{}{}
	}

	// Resolve parent_id for groups (parent must also be a group).
	groupByID := map[int64]*services.BackupMonitorGroup{}
	for i := range doc.MonitorGroups {
		groupByID[doc.MonitorGroups[i].ID] = &doc.MonitorGroups[i]
	}
	for _, m := range raw {
		if strings.ToLower(m.Type) != "group" {
			continue
		}
		if m.Parent.Valid && m.Parent.Int64 != 0 {
			if _, ok := groupIDs[m.Parent.Int64]; ok {
				pid := m.Parent.Int64
				groupByID[m.ID].ParentID = &pid
			} else {
				report.Skipped = append(report.Skipped, SkipReason{
					Kind: "monitor_group", ID: m.ID, Type: "group", Name: m.Name,
					Reason: fmt.Sprintf("parent id %d is not a group", m.Parent.Int64),
				})
			}
		}
	}

	// Second pass: checkable monitors.
	for _, m := range raw {
		if strings.ToLower(m.Type) == "group" {
			continue
		}
		// sqlserver is excluded from Phoenix (database engine is fine but
		// mssql as a monitor engine is not in the supported database engines).
		if strings.EqualFold(m.Type, "sqlserver") {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "monitor", ID: m.ID, Type: m.Type, Name: m.Name,
				Reason: "mssql (sqlserver) is explicitly excluded from Phoenix monitor types",
			})
			continue
		}

		phoenixType, reason := mapMonitorType(m.Type)
		if phoenixType == "" {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "monitor", ID: m.ID, Type: m.Type, Name: m.Name, Reason: reason,
			})
			continue
		}
		cfg, codes, err := buildMonitorConfig(phoenixType, m)
		if err != nil {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "monitor", ID: m.ID, Type: m.Type, Name: m.Name,
				Reason: err.Error(),
			})
			continue
		}

		bm := services.BackupMonitor{
			ID:                  m.ID,
			Name:                m.Name,
			Description:         m.Description,
			Type:                phoenixType,
			Active:              m.Active,
			Interval:            m.Interval,
			RetryInterval:       m.RetryInterval,
			MaxRetries:          m.MaxRetries,
			Timeout:             m.Timeout,
			Config:              cfg,
			AcceptedStatusCodes: codes,
			UpsideDown:          m.UpsideDown,
			ResendInterval:      m.ResendInterval,
			Weight:              m.Weight,
			TLSIgnore:           m.IgnoreTLS,
		}
		if phoenixType == "push" {
			bm.PushToken = m.PushToken
		}
		// Group membership: parent that is a group.
		if m.Parent.Valid && m.Parent.Int64 != 0 {
			if _, ok := groupIDs[m.Parent.Int64]; ok {
				pid := m.Parent.Int64
				bm.GroupID = &pid
			} else if _, isMon := findRawByID(raw, m.Parent.Int64); isMon {
				// Parent is another checkable monitor — Phoenix does not nest
				// monitors under monitors. Report and leave ungrouped.
				report.Skipped = append(report.Skipped, SkipReason{
					Kind: "monitor_parent", ID: m.ID, Type: m.Type, Name: m.Name,
					Reason: fmt.Sprintf("parent monitor %d is not a folder; Phoenix does not nest monitors under monitors", m.Parent.Int64),
				})
			}
		}
		// Proxy link preserved when present (import remaps).
		if p := m.ProxyID.Ptr(); p != nil {
			bm.ProxyID = p
		}

		// Sprint C F2.1: map Kuma expiry_notification when the column exists.
		bm.CertExpiryNotify = m.ExpiryNotification

		doc.Monitors = append(doc.Monitors, bm)
		monitorIDs[m.ID] = struct{}{}
	}
	return nil
}

func findRawByID(raw []*kumaMonitor, id int64) (*kumaMonitor, bool) {
	for _, m := range raw {
		if m.ID == id {
			return m, true
		}
	}
	return nil, false
}

func convertMonitorTags(db *sql.DB, doc *services.BackupDocument, report *Report, monitorIDs map[int64]struct{}) error {
	tagIDs := map[int64]struct{}{}
	for _, t := range doc.Tags {
		tagIDs[t.ID] = struct{}{}
	}
	rows, err := db.Query(`
		SELECT monitor_id, tag_id, COALESCE(value, '')
		FROM monitor_tag
		ORDER BY monitor_id ASC, tag_id ASC`)
	if err != nil {
		return fmt.Errorf("query monitor_tag: %w", err)
	}
	defer closeIgnoringError(rows)
	for rows.Next() {
		var mid, tid int64
		var value string
		if err := rows.Scan(&mid, &tid, &value); err != nil {
			return fmt.Errorf("scan monitor_tag: %w", err)
		}
		if _, ok := monitorIDs[mid]; !ok {
			continue // monitor was skipped
		}
		if _, ok := tagIDs[tid]; !ok {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "monitor_tag", ID: mid, Reason: fmt.Sprintf("tag id %d missing", tid),
			})
			continue
		}
		doc.MonitorTags = append(doc.MonitorTags, services.BackupMonitorTag{
			MonitorID: mid, TagID: tid, Value: value,
		})
	}
	return rows.Err()
}

func convertMonitorNotifications(
	db *sql.DB,
	doc *services.BackupDocument,
	report *Report,
	monitorIDs, notifIDs map[int64]struct{},
) error {
	rows, err := db.Query(`
		SELECT monitor_id, notification_id
		FROM monitor_notification
		ORDER BY monitor_id ASC, notification_id ASC`)
	if err != nil {
		return fmt.Errorf("query monitor_notification: %w", err)
	}
	defer closeIgnoringError(rows)
	for rows.Next() {
		var mid, nid int64
		if err := rows.Scan(&mid, &nid); err != nil {
			return fmt.Errorf("scan monitor_notification: %w", err)
		}
		if _, ok := monitorIDs[mid]; !ok {
			continue
		}
		if _, ok := notifIDs[nid]; !ok {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "monitor_notification", ID: mid,
				Reason: fmt.Sprintf("notification id %d was not imported", nid),
			})
			continue
		}
		doc.MonitorNotifications = append(doc.MonitorNotifications, services.BackupMonitorNotification{
			MonitorID: mid, NotificationID: nid,
		})
	}
	return rows.Err()
}

func convertStatusPages(src *source, doc *services.BackupDocument, monitorIDs map[int64]struct{}) error {
	db := src.db
	spCols, err := src.tableColumns("status_page")
	if err != nil {
		return err
	}
	// Build a flexible select.
	q := `SELECT id, COALESCE(slug, ''), COALESCE(title, ''), COALESCE(description, ''),
	             COALESCE(icon, ''), COALESCE(theme, 'auto'), COALESCE(published, 1),
	             COALESCE(password, ''), COALESCE(footer_text, ''), COALESCE(custom_css, ''),
	             COALESCE(show_tags, 0)`
	if hasCol(spCols, "show_certificate_expiry") {
		// ignored — Track B owns public cert display; no backup field yet for the toggle
		_ = true
	}
	q += ` FROM status_page ORDER BY id ASC`

	rows, err := db.Query(q)
	if err != nil {
		return fmt.Errorf("query status_page: %w", err)
	}
	defer closeIgnoringError(rows)

	spIDs := map[int64]struct{}{}
	for rows.Next() {
		var sp services.BackupStatusPage
		var published, showTags int
		if err := rows.Scan(
			&sp.ID, &sp.Slug, &sp.Title, &sp.Description, &sp.Icon, &sp.Theme,
			&published, &sp.PasswordHash, &sp.FooterText, &sp.CustomCSS, &showTags,
		); err != nil {
			return fmt.Errorf("scan status_page: %w", err)
		}
		sp.Published = published != 0
		sp.ShowTags = showTags != 0
		sp.DashboardStyle = domain.DashboardStyleFull
		sp.AutoResolveIncidents = false
		// Password in Kuma is stored as a hash when set; we pass it through
		// so a protected page stays protected after Phoenix import.
		doc.StatusPages = append(doc.StatusPages, sp)
		spIDs[sp.ID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// CNAMEs
	if ok, _ := src.tableExists("status_page_cname"); ok {
		crows, err := db.Query(`
			SELECT status_page_id, domain
			FROM status_page_cname
			ORDER BY status_page_id ASC, domain ASC`)
		if err != nil {
			return fmt.Errorf("query status_page_cname: %w", err)
		}
		defer closeIgnoringError(crows)
		for crows.Next() {
			var c services.BackupStatusPageCNAME
			if err := crows.Scan(&c.StatusPageID, &c.Domain); err != nil {
				return fmt.Errorf("scan status_page_cname: %w", err)
			}
			if _, ok := spIDs[c.StatusPageID]; !ok {
				continue
			}
			doc.StatusPageCNAMEs = append(doc.StatusPageCNAMEs, c)
		}
		if err := crows.Err(); err != nil {
			return err
		}
	}

	// Status-page monitor order via group + monitor_group (Kuma schema).
	// group.status_page_id links a status-page section to the page;
	// monitor_group links monitors into that section with a weight.
	// "group" is a reserved word — quote per dialect (SQLite "group", MariaDB `group`).
	if ok, _ := src.tableExists("group"); ok {
		if ok2, _ := src.tableExists("monitor_group"); ok2 {
			gcols, _ := src.tableColumns("group")
			if hasCol(gcols, "status_page_id") {
				groupTbl := src.quote("group")
				mrows, err := db.Query(fmt.Sprintf(`
					SELECT g.status_page_id, mg.monitor_id, COALESCE(mg.weight, 1000)
					FROM monitor_group mg
					JOIN %s g ON g.id = mg.group_id
					WHERE g.status_page_id IS NOT NULL
					ORDER BY g.status_page_id ASC, mg.weight ASC, mg.monitor_id ASC`, groupTbl))
				if err != nil {
					return fmt.Errorf("query status page monitors: %w", err)
				}
				defer closeIgnoringError(mrows)
				// Display order: dense rank per status page by weight.
				orderCounter := map[int64]int{}
				seen := map[string]struct{}{}
				for mrows.Next() {
					var spID, monID int64
					var weight int
					if err := mrows.Scan(&spID, &monID, &weight); err != nil {
						return fmt.Errorf("scan status page monitor: %w", err)
					}
					if _, ok := spIDs[spID]; !ok {
						continue
					}
					if _, ok := monitorIDs[monID]; !ok {
						continue
					}
					key := fmt.Sprintf("%d:%d", spID, monID)
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					ord := orderCounter[spID]
					orderCounter[spID] = ord + 1
					doc.StatusPageMonitors = append(doc.StatusPageMonitors, services.BackupStatusPageMonitor{
						StatusPageID: spID,
						MonitorID:    monID,
						DisplayOrder: ord,
					})
				}
				if err := mrows.Err(); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func convertMaintenance(src *source, doc *services.BackupDocument, report *Report, monitorIDs map[int64]struct{}) error {
	db := src.db
	cols, err := src.tableColumns("maintenance")
	if err != nil {
		return err
	}
	selectParts := []string{
		"id",
		"COALESCE(title, '')",
		"COALESCE(description, '')",
		"COALESCE(active, 1)",
		"COALESCE(strategy, 'single')",
		"start_date",
		"end_date",
	}
	hasCron := hasCol(cols, "cron")
	hasDuration := hasCol(cols, "duration")
	hasTimezone := hasCol(cols, "timezone")
	if hasCron {
		selectParts = append(selectParts, "COALESCE(cron, '')")
	}
	if hasDuration {
		selectParts = append(selectParts, "COALESCE(duration, 0)")
	}
	if hasTimezone {
		selectParts = append(selectParts, "COALESCE(timezone, '')")
	}

	q := `SELECT ` + strings.Join(selectParts, ", ") + ` FROM maintenance ORDER BY id ASC`
	rows, err := db.Query(q)
	if err != nil {
		return fmt.Errorf("query maintenance: %w", err)
	}
	defer closeIgnoringError(rows)

	kept := map[int64]struct{}{}
	for rows.Next() {
		var (
			id                 int64
			title, description string
			active             int
			strategy           string
			startRaw, endRaw   sql.NullString
			cron               string
			duration           int
			timezone           string
		)
		dest := []any{&id, &title, &description, &active, &strategy, &startRaw, &endRaw}
		if hasCron {
			dest = append(dest, &cron)
		}
		if hasDuration {
			dest = append(dest, &duration)
		}
		if hasTimezone {
			dest = append(dest, &timezone)
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan maintenance: %w", err)
		}

		start, _ := parseKumaTime(startRaw.String)
		end, _ := parseKumaTime(endRaw.String)
		// Only single/cron strategies map cleanly.
		strategyLower := strings.ToLower(strategy)
		if strategyLower != "single" && strategyLower != "cron" {
			report.Skipped = append(report.Skipped, SkipReason{
				Kind: "maintenance", ID: id, Name: title, Type: strategy,
				Reason: fmt.Sprintf("unsupported maintenance strategy %q", strategy),
			})
			continue
		}
		if timezone == "" {
			timezone = "UTC"
		}
		doc.MaintenanceWindows = append(doc.MaintenanceWindows, services.BackupMaintenance{
			ID:          id,
			Title:       title,
			Description: description,
			Active:      active != 0,
			Strategy:    strategyLower,
			StartDate:   start,
			EndDate:     end,
			CronExpr:    cron,
			Duration:    duration,
			Timezone:    timezone,
		})
		kept[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if ok, _ := src.tableExists("monitor_maintenance"); ok {
		mrows, err := db.Query(`
			SELECT maintenance_id, monitor_id
			FROM monitor_maintenance
			ORDER BY maintenance_id ASC, monitor_id ASC`)
		if err != nil {
			return fmt.Errorf("query monitor_maintenance: %w", err)
		}
		defer closeIgnoringError(mrows)
		for mrows.Next() {
			var maintID, monID int64
			if err := mrows.Scan(&maintID, &monID); err != nil {
				return fmt.Errorf("scan monitor_maintenance: %w", err)
			}
			if _, ok := kept[maintID]; !ok {
				continue
			}
			if _, ok := monitorIDs[monID]; !ok {
				continue
			}
			doc.MaintenanceMonitors = append(doc.MaintenanceMonitors, services.BackupMaintenanceMonitor{
				MaintenanceID: maintID,
				MonitorID:     monID,
			})
		}
		return mrows.Err()
	}
	return nil
}

func parseKumaTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.000",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unparsed time %q", s)
}

// sortDocument applies deterministic ordering to every slice.
func sortDocument(doc *services.BackupDocument) {
	sort.SliceStable(doc.Proxies, func(i, j int) bool { return doc.Proxies[i].ID < doc.Proxies[j].ID })
	sort.SliceStable(doc.Notifications, func(i, j int) bool { return doc.Notifications[i].ID < doc.Notifications[j].ID })
	sort.SliceStable(doc.Tags, func(i, j int) bool { return doc.Tags[i].ID < doc.Tags[j].ID })
	sort.SliceStable(doc.MonitorGroups, func(i, j int) bool { return doc.MonitorGroups[i].ID < doc.MonitorGroups[j].ID })
	sort.SliceStable(doc.Monitors, func(i, j int) bool { return doc.Monitors[i].ID < doc.Monitors[j].ID })
	sort.SliceStable(doc.MonitorTags, func(i, j int) bool {
		if doc.MonitorTags[i].MonitorID != doc.MonitorTags[j].MonitorID {
			return doc.MonitorTags[i].MonitorID < doc.MonitorTags[j].MonitorID
		}
		return doc.MonitorTags[i].TagID < doc.MonitorTags[j].TagID
	})
	sort.SliceStable(doc.MonitorNotifications, func(i, j int) bool {
		if doc.MonitorNotifications[i].MonitorID != doc.MonitorNotifications[j].MonitorID {
			return doc.MonitorNotifications[i].MonitorID < doc.MonitorNotifications[j].MonitorID
		}
		return doc.MonitorNotifications[i].NotificationID < doc.MonitorNotifications[j].NotificationID
	})
	sort.SliceStable(doc.StatusPages, func(i, j int) bool { return doc.StatusPages[i].ID < doc.StatusPages[j].ID })
	sort.SliceStable(doc.StatusPageMonitors, func(i, j int) bool {
		if doc.StatusPageMonitors[i].StatusPageID != doc.StatusPageMonitors[j].StatusPageID {
			return doc.StatusPageMonitors[i].StatusPageID < doc.StatusPageMonitors[j].StatusPageID
		}
		if doc.StatusPageMonitors[i].DisplayOrder != doc.StatusPageMonitors[j].DisplayOrder {
			return doc.StatusPageMonitors[i].DisplayOrder < doc.StatusPageMonitors[j].DisplayOrder
		}
		return doc.StatusPageMonitors[i].MonitorID < doc.StatusPageMonitors[j].MonitorID
	})
	sort.SliceStable(doc.StatusPageCNAMEs, func(i, j int) bool {
		if doc.StatusPageCNAMEs[i].StatusPageID != doc.StatusPageCNAMEs[j].StatusPageID {
			return doc.StatusPageCNAMEs[i].StatusPageID < doc.StatusPageCNAMEs[j].StatusPageID
		}
		return doc.StatusPageCNAMEs[i].Domain < doc.StatusPageCNAMEs[j].Domain
	})
	sort.SliceStable(doc.MaintenanceWindows, func(i, j int) bool {
		return doc.MaintenanceWindows[i].ID < doc.MaintenanceWindows[j].ID
	})
	sort.SliceStable(doc.MaintenanceMonitors, func(i, j int) bool {
		if doc.MaintenanceMonitors[i].MaintenanceID != doc.MaintenanceMonitors[j].MaintenanceID {
			return doc.MaintenanceMonitors[i].MaintenanceID < doc.MaintenanceMonitors[j].MaintenanceID
		}
		return doc.MaintenanceMonitors[i].MonitorID < doc.MaintenanceMonitors[j].MonitorID
	})
}

// Scan helpers: SQLite may return INTEGER for booleans.
type boolScanner struct{ dst *bool }

func boolDest(dst *bool) *boolScanner { return &boolScanner{dst: dst} }

func (b *boolScanner) Scan(src any) error {
	*b.dst = parseBoolish(src)
	return nil
}

type nullIntScanner struct{ dst *sqlNullInt }

func nullIntDest(dst *sqlNullInt) *nullIntScanner { return &nullIntScanner{dst: dst} }

func (n *nullIntScanner) Scan(src any) error {
	if src == nil {
		n.dst.Valid = false
		n.dst.Int64 = 0
		return nil
	}
	v, ok := asInt64(src)
	if !ok {
		n.dst.Valid = false
		return nil
	}
	n.dst.Valid = true
	n.dst.Int64 = v
	return nil
}
