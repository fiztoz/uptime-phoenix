package domain

import (
	"net/url"
	"regexp"
	"strings"
)

// displayTargetKeys lists, per monitor type, the config keys whose value is
// the monitor's target, in order of preference.
var displayTargetKeys = map[string][]string{
	"http":      {"url"},
	"websocket": {"url"},
	"tcp":       {"hostname", "host"},
	"ping":      {"hostname", "host"},
	"dns":       {"hostname", "host"},
	"grpc":      {"hostname"},
	"mqtt":      {"broker", "url", "hostname", "host"},
	"rabbitmq":  {"url", "connection_string", "dsn", "hostname", "host"},
	"snmp":      {"hostname", "host"},
}

// MonitorDisplayTarget is the target shown to anyone who can see the
// monitor: monitor cards over WebSocket, notification messages and alert
// contexts. It never contains credentials:
//   - s3 shows endpoint/bucket (S3DisplayTarget);
//   - database shows host[:port][/database] parsed out of the DSN
//     (DatabaseDisplayTarget), never the DSN itself;
//   - every other type shows its configured URL or host with any URL
//     userinfo removed.
func MonitorDisplayTarget(monitorType string, cfg map[string]any) string {
	switch monitorType {
	case "s3":
		return S3DisplayTarget(cfg)
	case "database":
		return DatabaseDisplayTarget(cfg)
	}
	for _, key := range displayTargetKeys[monitorType] {
		if v, ok := cfg[key].(string); ok && strings.TrimSpace(v) != "" {
			return stripURLUserinfo(strings.TrimSpace(v))
		}
	}
	return ""
}

// stripURLUserinfo removes user:password@ from a scheme://… URL. Anything
// that is not such a URL is returned unchanged.
func stripURLUserinfo(s string) string {
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok || !strings.Contains(rest, "@") {
		return s
	}
	if u, err := url.Parse(s); err == nil {
		u.User = nil
		return u.String()
	}
	// Unparseable (an unescaped character in the password, say): keep only
	// what follows the last '@', which may drop part of a path but can never
	// keep part of the userinfo.
	return scheme + "://" + rest[strings.LastIndex(rest, "@")+1:]
}

// DatabaseDisplayTarget returns host[:port][/database] for a database
// monitor. It understands URL DSNs (postgres://, mysql://, mongodb://,
// redis://, sqlserver://), Go MySQL DSNs (user:pass@tcp(host:3306)/db) and
// keyword DSNs (PostgreSQL "host=… dbname=… password=…", SQL Server
// "Server=…;Database=…;Password=…"). Credentials and query parameters are
// never included; a DSN it cannot parse yields "" rather than the raw text.
func DatabaseDisplayTarget(cfg map[string]any) string {
	if cfg == nil {
		return ""
	}
	for _, key := range []string{"connection_string", "dsn", "connectionString"} {
		if v, ok := cfg[key].(string); ok && strings.TrimSpace(v) != "" {
			return databaseDSNDisplay(strings.TrimSpace(v))
		}
	}
	if v, ok := cfg["hostname"].(string); ok {
		return stripURLUserinfo(strings.TrimSpace(v))
	}
	return ""
}

// keywordDSNKey matches the start of a keyword DSN: an identifier (spaces
// allowed, as in SQL Server's "Data Source") followed by '='.
var keywordDSNKey = regexp.MustCompile(`^\s*[A-Za-z][A-Za-z0-9 _.]*=`)

func databaseDSNDisplay(dsn string) string {
	if scheme, rest, ok := strings.Cut(dsn, "://"); ok {
		return urlDSNDisplay(scheme, rest)
	}
	if keywordDSNKey.MatchString(dsn) {
		return keywordDSNDisplay(dsn)
	}
	return mysqlDSNDisplay(dsn)
}

// urlDSNDisplay keeps scheme://host[:port][/path] of a URL DSN. Userinfo is
// cut at the LAST '@' (an unescaped '@' in a password must not leak the
// tail of it), and the query, which may carry password=, is dropped.
func urlDSNDisplay(scheme, rest string) string {
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimRight(rest, "/")
	if rest == "" {
		return ""
	}
	return scheme + "://" + rest
}

// mysqlDSNDisplay handles the go-sql-driver form
// [user[:password]@][net[(addr)]]/dbname[?params], splitting it the way the
// driver does: the database name follows the LAST '/', and the userinfo ends
// at the last '@' before it.
func mysqlDSNDisplay(dsn string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return ""
	}
	addr := dsn[:slash]
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		addr = addr[at+1:]
	}
	if open := strings.Index(addr, "("); open >= 0 && strings.HasSuffix(addr, ")") {
		addr = addr[open+1 : len(addr)-1]
	}
	db := dsn[slash+1:]
	if i := strings.Index(db, "?"); i >= 0 {
		db = db[:i]
	}
	return joinHostDB(addr, "", db)
}

// keywordDSNDisplay reads host, port and database out of a keyword DSN.
// Semicolon-separated (SQL Server / ADO) and space-separated (libpq, with
// single-quoted values) forms are both accepted.
func keywordDSNDisplay(dsn string) string {
	pairs := map[string]string{}
	if strings.Contains(dsn, ";") {
		for _, part := range strings.Split(dsn, ";") {
			if k, v, ok := strings.Cut(part, "="); ok {
				pairs[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
	} else {
		for _, kv := range splitLibpqDSN(dsn) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				pairs[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), "'")
			}
		}
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v := pairs[k]; v != "" {
				return v
			}
		}
		return ""
	}
	host := pick("host", "hostaddr", "server", "data source", "address", "addr", "network address")
	host = strings.TrimPrefix(host, "tcp:")
	return joinHostDB(host, pick("port"), pick("dbname", "database", "initial catalog"))
}

// splitLibpqDSN splits "k=v k2='v 2'" on unquoted whitespace.
func splitLibpqDSN(dsn string) []string {
	var out []string
	var cur strings.Builder
	quoted, escaped := false, false
	for _, r := range dsn {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\':
			cur.WriteRune(r)
			escaped = true
		case r == '\'':
			cur.WriteRune(r)
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\n'):
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func joinHostDB(host, port, db string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if port != "" && !strings.Contains(host, ":") && !strings.Contains(host, ",") {
		host += ":" + port
	}
	if db = strings.TrimSpace(db); db != "" {
		return host + "/" + db
	}
	return host
}
