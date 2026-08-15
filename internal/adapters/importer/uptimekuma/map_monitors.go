package uptimekuma

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Phoenix monitor types (locked set of 12).
var phoenixMonitorTypes = map[string]struct{}{
	"http": {}, "tcp": {}, "ping": {}, "dns": {}, "websocket": {},
	"push": {}, "docker": {}, "mqtt": {}, "rabbitmq": {}, "grpc": {}, "snmp": {},
	"database": {},
}

// mapMonitorType maps a Kuma monitor type to a Phoenix type.
// type "group" is handled separately as a folder, not a monitor.
// Returns ("", reason) when unsupported — never silently coerce.
func mapMonitorType(kumaType string) (phoenixType string, reason string) {
	t := strings.ToLower(strings.TrimSpace(kumaType))
	switch t {
	case "http", "keyword", "json-query":
		return "http", ""
	case "port":
		return "tcp", ""
	case "ping":
		return "ping", ""
	case "dns":
		return "dns", ""
	case "websocket", "websocket-keyword":
		return "websocket", ""
	case "push":
		return "push", ""
	case "docker":
		return "docker", ""
	case "mqtt":
		return "mqtt", ""
	case "rabbitmq":
		return "rabbitmq", ""
	case "grpc", "grpc-keyword":
		return "grpc", ""
	case "snmp":
		return "snmp", ""
	case "sqlserver", "postgres", "postgresql", "mysql", "mariadb", "mongodb", "mongo", "redis":
		return "database", ""
	case "group":
		return "", "group monitors become folders, not checkable monitors"
	case "systemd", "system-service", "systemservice":
		return "", "systemd is deferred until Phoenix agent (local D-Bus only; not usable remotely)"
	case "gamedig", "steam":
		return "", "gamedig/steam is explicitly excluded from Phoenix monitor types"
	case "tailscale":
		return "", "tailscale is explicitly excluded from Phoenix monitor types"
	case "radius":
		return "", "radius is explicitly excluded from Phoenix monitor types"
	case "kafka-producer", "kafka":
		return "", "kafka is explicitly excluded from Phoenix monitor types"
	case "real-browser":
		return "", "real-browser has no Phoenix equivalent"
	case "manual", "globalping", "global-ping":
		return "", fmt.Sprintf("unsupported Kuma monitor type %q", kumaType)
	case "":
		return "", "monitor type missing"
	default:
		return "", fmt.Sprintf("unsupported Kuma monitor type %q (Phoenix supports 12 types only)", kumaType)
	}
}

// kumaMonitor is a row from the Kuma monitor table (subset of columns).
type kumaMonitor struct {
	ID                       int64
	Name                     string
	Description              string
	Type                     string
	Active                   bool
	Interval                 int
	RetryInterval            int
	MaxRetries               int
	Timeout                  float64
	URL                      string
	Hostname                 string
	Port                     sqlNullInt
	Keyword                  string
	IgnoreTLS                bool
	UpsideDown               bool
	MaxRedirects             int
	AcceptedStatuscodesJSON  string
	DNSResolveType           string
	DNSResolveServer         string
	PushToken                string
	Method                   string
	Body                     string
	Headers                  string
	DockerContainer          string
	ProxyID                  sqlNullInt
	ExpiryNotification       bool
	MQTTTopic                string
	MQTTSuccessMessage       string
	MQTTUsername             string
	MQTTPassword             string
	DatabaseConnectionString string
	DatabaseQuery            string
	GRPCURL                  string
	GRPCServiceName          string
	GRPCEnableTLS            bool
	ResendInterval           int
	Game                     string
	Parent                   sqlNullInt
	JSONPath                 string
	ExpectedValue            string
	Weight                   int
	SystemServiceName        string
	SNMPOid                  string
	SNMPVersion              string
}

// sqlNullInt is a tiny helper for optional integer columns.
type sqlNullInt struct {
	Int64 int64
	Valid bool
}

func (n sqlNullInt) Ptr() *int64 {
	if !n.Valid || n.Int64 == 0 {
		return nil
	}
	v := n.Int64
	return &v
}

// buildMonitorConfig builds a Phoenix checker config map for the given
// mapped type. Only keys the Phoenix checker understands are emitted.
func buildMonitorConfig(phoenixType string, m *kumaMonitor) (map[string]any, []string, error) {
	cfg := make(map[string]any)
	var codes []string

	// Accepted status codes apply primarily to HTTP.
	if m.AcceptedStatuscodesJSON != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(m.AcceptedStatuscodesJSON), &parsed); err == nil {
			codes = parsed
		}
	}
	if len(codes) == 0 && phoenixType == "http" {
		codes = []string{"200-299"}
	}

	switch phoenixType {
	case "http":
		if strings.TrimSpace(m.URL) == "" {
			return nil, nil, fmt.Errorf("http monitor requires url")
		}
		cfg["url"] = m.URL
		if m.Method != "" {
			cfg["method"] = strings.ToUpper(m.Method)
		}
		if m.Body != "" {
			cfg["body"] = m.Body
		}
		if m.Headers != "" {
			// Kuma stores headers as a JSON object string.
			var hdr map[string]any
			if err := json.Unmarshal([]byte(m.Headers), &hdr); err == nil {
				cfg["headers"] = hdr
			} else {
				cfg["headers"] = m.Headers
			}
		}
		if m.Keyword != "" {
			cfg["keyword"] = m.Keyword
		}
		if m.JSONPath != "" {
			cfg["json_query"] = m.JSONPath
			if m.ExpectedValue != "" {
				cfg["json_operator"] = "equals"
				cfg["expected_value"] = m.ExpectedValue
			}
		}
		// maxredirects 0 → do not follow; Phoenix uses follow_redirects bool.
		if m.MaxRedirects == 0 {
			cfg["follow_redirects"] = false
		} else {
			cfg["follow_redirects"] = true
		}
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "tcp":
		host := firstNonEmpty(m.Hostname, m.URL)
		if host == "" {
			return nil, nil, fmt.Errorf("tcp monitor requires hostname")
		}
		cfg["hostname"] = host
		if !m.Port.Valid {
			return nil, nil, fmt.Errorf("tcp monitor requires port")
		}
		cfg["port"] = float64(m.Port.Int64)
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "ping":
		host := firstNonEmpty(m.Hostname, m.URL)
		if host == "" {
			return nil, nil, fmt.Errorf("ping monitor requires hostname")
		}
		cfg["hostname"] = host
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "dns":
		host := firstNonEmpty(m.Hostname, m.URL)
		if host == "" {
			return nil, nil, fmt.Errorf("dns monitor requires hostname")
		}
		cfg["hostname"] = host
		if m.DNSResolveType != "" {
			cfg["resolve_type"] = m.DNSResolveType
		}
		if m.DNSResolveServer != "" {
			cfg["resolve_server"] = m.DNSResolveServer
		}
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "websocket":
		if strings.TrimSpace(m.URL) == "" {
			return nil, nil, fmt.Errorf("websocket monitor requires url")
		}
		cfg["url"] = m.URL
		if m.Keyword != "" {
			cfg["keyword"] = m.Keyword
		}
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "push":
		if m.PushToken == "" {
			return nil, nil, fmt.Errorf("push monitor requires push_token")
		}
		cfg["push_token"] = m.PushToken
	case "docker":
		container := m.DockerContainer
		if container == "" {
			return nil, nil, fmt.Errorf("docker monitor requires container")
		}
		cfg["container"] = container
	case "mqtt":
		// Kuma stores broker in hostname/port or url.
		broker := m.URL
		if broker == "" {
			host := m.Hostname
			if host == "" {
				return nil, nil, fmt.Errorf("mqtt monitor requires broker")
			}
			if m.Port.Valid {
				broker = fmt.Sprintf("tcp://%s:%d", host, m.Port.Int64)
			} else {
				broker = "tcp://" + host
			}
		}
		cfg["broker"] = broker
		if m.MQTTTopic != "" {
			cfg["topic"] = m.MQTTTopic
		}
		if m.MQTTSuccessMessage != "" {
			cfg["success_message"] = m.MQTTSuccessMessage
		}
		if m.MQTTUsername != "" {
			cfg["username"] = m.MQTTUsername
		}
		if m.MQTTPassword != "" {
			cfg["password"] = m.MQTTPassword
		}
	case "rabbitmq":
		amqpURL := m.URL
		if amqpURL == "" {
			host := m.Hostname
			if host == "" {
				return nil, nil, fmt.Errorf("rabbitmq monitor requires url or hostname")
			}
			port := int64(5672)
			if m.Port.Valid && m.Port.Int64 > 0 {
				port = m.Port.Int64
			}
			u := &url.URL{Scheme: "amqp", Host: net.JoinHostPort(host, strconv.FormatInt(port, 10))}
			if m.MQTTUsername != "" {
				u.User = url.UserPassword(m.MQTTUsername, m.MQTTPassword)
			}
			amqpURL = u.String()
		}
		cfg["url"] = amqpURL
		if m.Timeout > 0 {
			cfg["timeout"] = m.Timeout
		}
	case "grpc":
		url := firstNonEmpty(m.GRPCURL, m.URL, m.Hostname)
		if url == "" {
			return nil, nil, fmt.Errorf("grpc monitor requires url")
		}
		if m.Port.Valid && !strings.Contains(url, ":") {
			url = fmt.Sprintf("%s:%d", url, m.Port.Int64)
		}
		cfg["url"] = url
		if m.GRPCServiceName != "" {
			cfg["service_name"] = m.GRPCServiceName
		}
		if m.GRPCEnableTLS {
			cfg["tls"] = true
		}
	case "snmp":
		host := firstNonEmpty(m.Hostname, m.URL)
		if host == "" {
			return nil, nil, fmt.Errorf("snmp monitor requires hostname")
		}
		cfg["hostname"] = host
		oid := m.SNMPOid
		if oid == "" {
			return nil, nil, fmt.Errorf("snmp monitor requires oid")
		}
		cfg["oid"] = oid
		if m.SNMPVersion != "" {
			cfg["version"] = m.SNMPVersion
		}
		if m.Port.Valid {
			cfg["port"] = float64(m.Port.Int64)
		}
	case "database":
		engine := databaseEngineFromKumaType(m.Type)
		conn := m.DatabaseConnectionString
		if conn == "" {
			return nil, nil, fmt.Errorf("database monitor requires connection_string")
		}
		cfg["engine"] = engine
		cfg["connection_string"] = conn
		// Free-form SQL is not executed by Phoenix (injection surface). Any Kuma
		// query maps to the fixed select_1 health preset only.
		if m.DatabaseQuery != "" {
			cfg["health_check"] = "select_1"
		}
	default:
		return nil, nil, fmt.Errorf("no config builder for type %q", phoenixType)
	}
	return cfg, codes, nil
}

func databaseEngineFromKumaType(kumaType string) string {
	switch strings.ToLower(kumaType) {
	case "sqlserver":
		return "mssql"
	case "postgres", "postgresql":
		return "postgres"
	case "mysql":
		return "mysql"
	case "mariadb":
		return "mariadb"
	case "mongodb", "mongo":
		return "mongodb"
	case "redis":
		return "redis"
	default:
		return strings.ToLower(kumaType)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// parseBoolish converts SQLite 0/1, bool, or string into bool.
func parseBoolish(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case int64:
		return t != 0
	case int:
		return t != 0
	case float64:
		return t != 0
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	case []byte:
		return parseBoolish(string(t))
	default:
		return false
	}
}

// asInt64 coerces common SQLite driver types to int64.
func asInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	case []byte:
		return asInt64(string(t))
	default:
		return 0, false
	}
}
