package checker

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestCapacityConfigBool(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
		want bool
	}{
		{name: "missing", cfg: map[string]any{}, want: false},
		{name: "nil map", cfg: nil, want: false},
		{name: "bool true", cfg: map[string]any{"check_session_pool": true}, want: true},
		{name: "bool false", cfg: map[string]any{"check_session_pool": false}, want: false},
		{name: "string true", cfg: map[string]any{"check_session_pool": "true"}, want: true},
		{name: "string TRUE", cfg: map[string]any{"check_session_pool": "TRUE"}, want: true},
		{name: "string false", cfg: map[string]any{"check_session_pool": "false"}, want: false},
		{name: "string 1", cfg: map[string]any{"check_session_pool": "1"}, want: true},
		{name: "string 0", cfg: map[string]any{"check_session_pool": "0"}, want: false},
		{name: "int 1", cfg: map[string]any{"check_session_pool": 1}, want: true},
		{name: "int 0", cfg: map[string]any{"check_session_pool": 0}, want: false},
		{name: "int64 1", cfg: map[string]any{"check_session_pool": int64(1)}, want: true},
		{name: "float 1", cfg: map[string]any{"check_session_pool": 1.0}, want: true},
		{name: "float 0", cfg: map[string]any{"check_session_pool": 0.0}, want: false},
		{name: "json.Number 1", cfg: map[string]any{"check_session_pool": json.Number("1")}, want: true},
		{name: "json.Number 0", cfg: map[string]any{"check_session_pool": json.Number("0")}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configBool(tt.cfg, "check_session_pool"); got != tt.want {
				t.Errorf("configBool() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCapacityConfigFloat(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, ok, err := configFloat(map[string]any{}, "storage_threshold")
		if err != nil || ok {
			t.Fatalf("missing: ok=%v err=%v", ok, err)
		}
	})
	t.Run("float64", func(t *testing.T) {
		v, ok, err := configFloat(map[string]any{"storage_threshold": 80.5}, "storage_threshold")
		if err != nil || !ok || v != 80.5 {
			t.Fatalf("got %v ok=%v err=%v", v, ok, err)
		}
	})
	t.Run("int", func(t *testing.T) {
		v, ok, err := configFloat(map[string]any{"storage_threshold": 80}, "storage_threshold")
		if err != nil || !ok || v != 80 {
			t.Fatalf("got %v ok=%v err=%v", v, ok, err)
		}
	})
	t.Run("int64", func(t *testing.T) {
		v, ok, err := configFloat(map[string]any{"storage_threshold": int64(80)}, "storage_threshold")
		if err != nil || !ok || v != 80 {
			t.Fatalf("got %v ok=%v err=%v", v, ok, err)
		}
	})
	t.Run("numeric string", func(t *testing.T) {
		v, ok, err := configFloat(map[string]any{"storage_threshold": "80"}, "storage_threshold")
		if err != nil || !ok || v != 80 {
			t.Fatalf("got %v ok=%v err=%v", v, ok, err)
		}
	})
	t.Run("json.Number", func(t *testing.T) {
		v, ok, err := configFloat(map[string]any{"storage_threshold": json.Number("80.5")}, "storage_threshold")
		if err != nil || !ok || v != 80.5 {
			t.Fatalf("got %v ok=%v err=%v", v, ok, err)
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		_, ok, err := configFloat(map[string]any{"storage_threshold": "nope"}, "storage_threshold")
		if err == nil || !ok {
			t.Fatalf("want error with present=true, ok=%v err=%v", ok, err)
		}
	})
}

func TestCapacityParseOptsDefaults(t *testing.T) {
	opts := parseDBCapacityOpts(nil)
	if opts.CheckSessionPool || opts.CheckStorage {
		t.Fatalf("checks should default false: %+v", opts)
	}
	if opts.SessionPoolThreshold != 80 || opts.StorageThreshold != 80 {
		t.Fatalf("thresholds should default 80: %+v", opts)
	}
	if opts.HasStorageMaxGB {
		t.Fatalf("storage_max_gb should be unset: %+v", opts)
	}
	if opts.StorageKind != storageKindStorage {
		t.Fatalf("StorageKind = %q, want %q", opts.StorageKind, storageKindStorage)
	}

	opts = parseDBCapacityOpts(map[string]any{
		"check_session_pool":     true,
		"session_pool_threshold": 0.0,
		"check_storage":          "true",
		"storage_threshold":      "0",
	})
	if !opts.CheckSessionPool || !opts.CheckStorage {
		t.Fatalf("enabled flags not parsed: %+v", opts)
	}
	if opts.SessionPoolThreshold != 80 || opts.StorageThreshold != 80 {
		t.Fatalf("0 threshold should default to 80: %+v", opts)
	}

	opts = parseDBCapacityOpts(map[string]any{
		"session_pool_threshold": 75.0,
		"storage_threshold":      json.Number("90"),
		"storage_max_gb":         10,
	})
	if opts.SessionPoolThreshold != 75 || opts.StorageThreshold != 90 {
		t.Fatalf("custom thresholds: %+v", opts)
	}
	if !opts.HasStorageMaxGB || opts.StorageMaxGB != 10 {
		t.Fatalf("storage_max_gb: %+v", opts)
	}
}

func TestCapacityUsagePercent_MaxNonPositive(t *testing.T) {
	if _, err := usagePercent(usage{Used: 1, Max: 0}); err == nil {
		t.Fatal("expected error when Max=0")
	}
	if _, err := usagePercent(usage{Used: 1, Max: -1}); err == nil {
		t.Fatal("expected error when Max<0")
	}
	pct, err := usagePercent(usage{Used: 12, Max: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pct != 12 {
		t.Fatalf("pct = %v, want 12", pct)
	}
}

func TestCapacityThresholdReached(t *testing.T) {
	if !thresholdReached(80, 80) {
		t.Fatal("80/80 should be reached")
	}
	if thresholdReached(79.9, 80) {
		t.Fatal("79.9/80 should not be reached")
	}
	if !thresholdReached(80.1, 80) {
		t.Fatal("80.1/80 should be reached")
	}
}

func TestCapacityGiBConversion(t *testing.T) {
	if bytesPerGiB != 1024*1024*1024 {
		t.Fatalf("bytesPerGiB = %d, want 1024^3", bytesPerGiB)
	}
	if got := gibToBytes(1); got != 1073741824 {
		t.Errorf("gibToBytes(1) = %v, want 1073741824", got)
	}
	if got := bytesToGiB(1073741824); got != 1 {
		t.Errorf("bytesToGiB(1073741824) = %v, want 1", got)
	}
	// Official MariaDB DISKS example: 26203116 KiB ≈ 25 GiB. Must *1024 before /1024^3.
	const disksUsedKiB = 26203116.0
	got := bytesToGiB(disksUsedKiB * 1024)
	if got < 24.9 || got > 25.1 {
		t.Fatalf("DISKS KiB→GiB = %v, want ~25", got)
	}
}

func TestMariaDBDisksQueryDedupesDevice(t *testing.T) {
	if !strings.Contains(sqlMariaDBDisks, "GROUP BY DISK") {
		t.Fatal("information_schema.DISKS repeats each device per mount path; query must GROUP BY DISK")
	}
}

func TestCapacityFormatUsageAndBreach(t *testing.T) {
	sess := usage{Used: 92, Max: 100}
	if got := formatUsage("session pool", sess, 92.0, false); got != "session pool 92/100 (92.0%)" {
		t.Errorf("formatUsage session = %q", got)
	}
	if got := formatBreach("session pool", sess, 92.0, 80, false); got != "session pool 92/100 (92.0%) exceeds threshold 80%" {
		t.Errorf("formatBreach session = %q", got)
	}

	stor := usage{Used: 4.2 * bytesPerGiB, Max: 5 * bytesPerGiB}
	if got := formatUsage("storage", stor, 84.0, true); got != "storage 4.2/5.0 GiB (84.0%)" {
		t.Errorf("formatUsage storage = %q", got)
	}
	if got := formatBreach("storage", stor, 84.0, 80, true); got != "storage 4.2/5.0 GiB (84.0%) exceeds threshold 80%" {
		t.Errorf("formatBreach storage = %q", got)
	}
	if got := formatBreach("memory", usage{Used: 1.2 * bytesPerGiB, Max: 2 * bytesPerGiB}, 60.0, 50, true); got != "memory 1.2/2.0 GiB (60.0%) exceeds threshold 50%" {
		t.Errorf("formatBreach memory = %q", got)
	}
	if got := formatUsage("sessions", usage{Used: 12, Max: 100}, 12.0, false); got != "sessions 12/100 (12.0%)" {
		t.Errorf("formatUsage sessions = %q", got)
	}
}

func TestCapacityParseRedisInfoMap(t *testing.T) {
	raw := "# Clients\r\nconnected_clients:12\r\nmaxclients:100\r\nblocked_clients:0\r\n\r\n# Memory\r\nused_memory:1234\r\nmaxmemory:0\r\n"
	m := parseRedisInfoMap(raw)
	if m["connected_clients"] != "12" || m["maxclients"] != "100" {
		t.Fatalf("clients: %+v", m)
	}
	if m["used_memory"] != "1234" || m["maxmemory"] != "0" {
		t.Fatalf("memory: %+v", m)
	}
	if _, ok := m["# Clients"]; ok {
		t.Fatal("section headers should be skipped")
	}
	used, ok := parseRedisInfoFloat(m, "connected_clients")
	if !ok || used != 12 {
		t.Fatalf("parse connected_clients: %v ok=%v", used, ok)
	}
}

func TestApplyCapacityChecks(t *testing.T) {
	up := ports.CheckResult{Status: domain.StatusUp, Message: "connected, SELECT 1 ok"}
	off := dbCapacityOpts{SessionPoolThreshold: 80, StorageThreshold: 80}

	t.Run("both disabled leaves UP unchanged", func(t *testing.T) {
		got := applyCapacityChecks(up, &usage{Used: 99, Max: 100}, nil, &usage{Used: 9, Max: 10}, nil, off)
		if got.Status != domain.StatusUp || got.Message != up.Message {
			t.Fatalf("status=%v message=%q", got.Status, got.Message)
		}
		if got.Metadata != nil {
			t.Fatalf("metadata should stay nil, got %+v", got.Metadata)
		}
	})

	t.Run("session breach is warning while connectivity remains UP", func(t *testing.T) {
		opts := dbCapacityOpts{Engine: "postgres", CheckSessionPool: true, SessionPoolThreshold: 80, StorageThreshold: 80}
		got := applyCapacityChecks(up, &usage{Used: 92, Max: 100}, nil, nil, nil, opts)
		if got.Status != domain.StatusUp {
			t.Fatalf("status=%v, want UP", got.Status)
		}
		want := "connected, SELECT 1 ok; session pool 92/100 (92.0%) exceeds threshold 80%"
		if got.Message != want {
			t.Fatalf("message=%q want %q", got.Message, want)
		}
		if len(got.Conditions) != 1 || got.Conditions[0].State != domain.ConditionStateWarning {
			t.Fatalf("conditions=%+v, want one warning", got.Conditions)
		}
		if got.Conditions[0].Kind != domain.MonitorConditionSessionPool || got.Conditions[0].Scope != "cluster" {
			t.Fatalf("condition semantics=%+v", got.Conditions[0])
		}
		if got.Metadata != nil {
			t.Fatalf("capacity must not write Metadata, got %+v", got.Metadata)
		}
	})

	t.Run("storage breach is warning while connectivity remains UP", func(t *testing.T) {
		opts := dbCapacityOpts{Engine: "mysql", CheckStorage: true, SessionPoolThreshold: 80, StorageThreshold: 80}
		stor := &usage{Used: 4.2 * bytesPerGiB, Max: 5 * bytesPerGiB}
		got := applyCapacityChecks(up, nil, nil, stor, nil, opts)
		if got.Status != domain.StatusUp {
			t.Fatalf("status=%v, want UP", got.Status)
		}
		want := "connected, SELECT 1 ok; storage 4.2/5.0 GiB (84.0%) exceeds threshold 80%"
		if got.Message != want {
			t.Fatalf("message=%q want %q", got.Message, want)
		}
		if len(got.Conditions) != 1 || got.Conditions[0].State != domain.ConditionStateWarning || got.Conditions[0].Resource != "Database size" {
			t.Fatalf("conditions=%+v", got.Conditions)
		}
	})

	t.Run("query error is an explicit condition error", func(t *testing.T) {
		opts := dbCapacityOpts{CheckSessionPool: true, SessionPoolThreshold: 80}
		got := applyCapacityChecks(up, nil, errors.New("permission denied"), nil, nil, opts)
		if got.Status != domain.StatusUp {
			t.Fatalf("status=%v, want UP", got.Status)
		}
		want := "connected, SELECT 1 ok; session pool check failed: permission denied"
		if got.Message != want {
			t.Fatalf("message=%q want %q", got.Message, want)
		}
		if len(got.Conditions) != 1 || got.Conditions[0].State != domain.ConditionStateError {
			t.Fatalf("conditions=%+v, want one error", got.Conditions)
		}
	})

	t.Run("combines independent conditions", func(t *testing.T) {
		opts := dbCapacityOpts{
			Engine:               "mssql",
			CheckSessionPool:     true,
			SessionPoolThreshold: 80,
			CheckStorage:         true,
			StorageThreshold:     80,
		}
		stor := &usage{Used: 4.2 * bytesPerGiB, Max: 5 * bytesPerGiB}
		got := applyCapacityChecks(up, nil, errors.New("VIEW SERVER STATE"), stor, nil, opts)
		if got.Status != domain.StatusUp || len(got.Conditions) != 2 {
			t.Fatalf("status=%v conditions=%+v", got.Status, got.Conditions)
		}
		if got.Conditions[0].State != domain.ConditionStateError || got.Conditions[1].State != domain.ConditionStateWarning {
			t.Fatalf("conditions=%+v", got.Conditions)
		}
	})

	t.Run("does not overwrite DOWN base", func(t *testing.T) {
		down := ports.CheckResult{Status: domain.StatusDown, Message: "ping failed: nope"}
		opts := dbCapacityOpts{CheckSessionPool: true, SessionPoolThreshold: 80}
		got := applyCapacityChecks(down, &usage{Used: 99, Max: 100}, nil, nil, nil, opts)
		if got.Status != domain.StatusDown || got.Message != down.Message {
			t.Fatalf("status=%v message=%q", got.Status, got.Message)
		}
		if len(got.Conditions) != 0 {
			t.Fatalf("primary failure must not claim capacity observations: %+v", got.Conditions)
		}
	})

	t.Run("success appends metrics", func(t *testing.T) {
		opts := dbCapacityOpts{
			CheckSessionPool:     true,
			SessionPoolThreshold: 80,
			CheckStorage:         true,
			StorageThreshold:     80,
		}
		stor := &usage{Used: 4.2 * bytesPerGiB, Max: 20 * bytesPerGiB}
		got := applyCapacityChecks(up, &usage{Used: 12, Max: 100}, nil, stor, nil, opts)
		if got.Status != domain.StatusUp {
			t.Fatalf("status=%v, want UP", got.Status)
		}
		want := "connected, SELECT 1 ok; sessions 12/100 (12.0%); storage 4.2/20.0 GiB (21.0%)"
		if got.Message != want {
			t.Fatalf("message=%q want %q", got.Message, want)
		}
		if got.Metadata != nil {
			t.Fatalf("capacity must not write Metadata, got %+v", got.Metadata)
		}
		if len(got.Conditions) != 2 || got.Conditions[0].State != domain.ConditionStateOK || got.Conditions[1].State != domain.ConditionStateOK {
			t.Fatalf("conditions=%+v, want two OK observations", got.Conditions)
		}
	})

	t.Run("redis memory wording", func(t *testing.T) {
		opts := dbCapacityOpts{
			Engine:           "redis",
			CheckStorage:     true,
			StorageThreshold: 50,
			StorageKind:      storageKindMemory,
		}
		mem := &usage{Used: 1.2 * bytesPerGiB, Max: 2 * bytesPerGiB}
		got := applyCapacityChecks(up, nil, nil, mem, nil, opts)
		if got.Status != domain.StatusUp || len(got.Conditions) != 1 {
			t.Fatalf("status=%v conditions=%+v", got.Status, got.Conditions)
		}
		if got.Conditions[0].State != domain.ConditionStateWarning || got.Conditions[0].Resource != "Redis memory" {
			t.Fatalf("condition=%+v", got.Conditions[0])
		}
	})

	t.Run("Max 0 after query is explicit error", func(t *testing.T) {
		opts := dbCapacityOpts{CheckSessionPool: true, SessionPoolThreshold: 80}
		got := applyCapacityChecks(up, &usage{Used: 5, Max: 0}, nil, nil, nil, opts)
		if got.Status != domain.StatusUp || len(got.Conditions) != 1 || got.Conditions[0].State != domain.ConditionStateError {
			t.Fatalf("status=%v conditions=%+v", got.Status, got.Conditions)
		}
	})
}

func TestCapacityAnyToFloat(t *testing.T) {
	if got, ok := anyToFloat(int32(42)); !ok || got != 42 {
		t.Fatalf("int32: %v ok=%v", got, ok)
	}
	if got, ok := anyToFloat("not-a-number"); ok {
		t.Fatalf("string garbage should fail, got %v", got)
	}
	if got, ok := anyToFloat("1234.5"); !ok || got != 1234.5 {
		t.Fatalf("numeric string: %v ok=%v", got, ok)
	}
}

func TestCapacityMongoDBNameFromURI(t *testing.T) {
	if got := mongoDBNameFromURI("mongodb://host:27017/appdb?authSource=admin"); got != "appdb" {
		t.Fatalf("got %q", got)
	}
	if got := mongoDBNameFromURI("mongodb://host:27017"); got != "admin" {
		t.Fatalf("got %q", got)
	}
	if got := mongoDBNameFromURI("mongodb://host:27017/"); got != "admin" {
		t.Fatalf("got %q", got)
	}
}
