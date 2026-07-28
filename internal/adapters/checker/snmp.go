package checker

import (
	"context"
	"fmt"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SNMPChecker performs SNMP GET queries against an SNMP agent.
type SNMPChecker struct{}

func init() { Register(SNMPChecker{}) }

// Type returns the monitor type identifier.
func (SNMPChecker) Type() string { return "snmp" }

// Validate checks that required config fields are present before saving.
func (SNMPChecker) Validate(config map[string]any) error {
	hostname, _ := config["hostname"].(string)
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	oid, _ := config["oid"].(string)
	if oid == "" {
		return fmt.Errorf("oid is required")
	}
	return nil
}

// Check performs an SNMP GET request for the configured OID.
func (SNMPChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	// Extract config with defaults.
	hostname, _ := config["hostname"].(string)
	oid, _ := config["oid"].(string)

	port := 161
	if portVal, ok := config["port"].(float64); ok && portVal > 0 {
		port = int(portVal)
	}

	community := "public"
	if c, ok := config["community"].(string); ok && c != "" {
		community = c
	}

	version := "2c"
	if v, ok := config["version"].(string); ok && v != "" {
		version = v
	}

	timeoutSec := 10.0
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}

	// Parse SNMP version.
	var snmpVersion gosnmp.SnmpVersion
	switch version {
	case "1":
		snmpVersion = gosnmp.Version1
	case "3":
		snmpVersion = gosnmp.Version3
	default:
		snmpVersion = gosnmp.Version2c
	}

	// Apply context timeout and propagate it to the gosnmp client so
	// Connect/Get honor cancellation and deadlines.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	snmpClient := &gosnmp.GoSNMP{
		Target:    hostname,
		Port:      uint16(port),
		Community: community,
		Version:   snmpVersion,
		Timeout:   time.Duration(timeoutSec * float64(time.Second)),
		Context:   ctx,
	}

	// Measure total check latency from start.
	start := time.Now()

	// Connect.
	if err := snmpClient.Connect(); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("snmp connect failed: %v", err),
		}, nil
	}
	defer func() { _ = snmpClient.Close() }()

	// Perform GET.
	result, err := snmpClient.Get([]string{oid})
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("snmp get failed: %v", err),
		}, nil
	}

	// Extract value from the first result variable.
	if len(result.Variables) == 0 {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   "snmp get returned no variables",
		}, nil
	}

	variable := result.Variables[0]
	valueStr := formatPDUValue(variable)

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   fmt.Sprintf("OID value: %s", valueStr),
		Metadata:  map[string]string{"oid": oid, "value": valueStr},
	}, nil
}

// formatPDUValue formats an SNMP PDU variable value as a human-readable string.
func formatPDUValue(variable gosnmp.SnmpPDU) string {
	switch variable.Type {
	case gosnmp.OctetString:
		return string(variable.Value.([]byte))
	case gosnmp.ObjectIdentifier:
		return variable.Value.(string)
	case gosnmp.Integer:
		return fmt.Sprintf("%d", variable.Value.(int))
	case gosnmp.Counter32, gosnmp.Counter64:
		return fmt.Sprintf("%d", variable.Value)
	case gosnmp.Gauge32, gosnmp.Uinteger32:
		return fmt.Sprintf("%d", variable.Value)
	case gosnmp.IPAddress:
		return variable.Value.(string)
	case gosnmp.TimeTicks:
		return fmt.Sprintf("%d ticks", variable.Value.(uint32))
	case gosnmp.Null:
		return "<NULL>"
	default:
		return fmt.Sprintf("%v", variable.Value)
	}
}
