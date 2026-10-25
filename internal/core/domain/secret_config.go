package domain

import (
	"net/url"
	"strings"
)

// SecretConfigPlaceholder replaces a write-only config value in API responses.
// Sending it back on update keeps the stored value. It is the same marker the
// config-as-code export uses, so both surfaces read the same way.
const SecretConfigPlaceholder = "__REDACTED__"

// secretConfigKeys are monitor and notification config keys whose values are
// credentials. Keys are compared after lower-casing and mapping '-' to '_'.
var secretConfigKeys = map[string]struct{}{
	"token": {}, "password": {}, "webhook_url": {}, "webhookurl": {}, "webhook": {},
	"bot_token": {}, "bottoken": {}, "api_key": {}, "apikey": {},
	"secret": {}, "auth_password": {}, "authpassword": {},
	"client_secret": {}, "clientsecret": {}, "access_token": {},
	"accesstoken": {}, "private_key": {}, "privatekey": {},
	"smtp_password": {}, "smtppassword": {}, "app_secret": {},
	"appsecret": {}, "channel_secret": {}, "channelsecret": {},
	"secret_key": {}, "secretkey": {}, "session_token": {}, "sessiontoken": {},
	"push_token": {}, "pushtoken": {},
	// Keys read by checkers and notifiers that the export list did not cover.
	"auth_token": {}, "auth_bearer_token": {}, "oauth2_client_secret": {},
	"basic_auth_pass": {}, "basic_auth_password": {},
	"connection_string": {}, "connectionstring": {}, "dsn": {}, "community": {},
	"app_token": {}, "channel_access_token": {}, "device_key": {},
	"access_key": {}, "routing_key": {}, "integration_key": {},
}

// IsSecretConfigKey reports whether a config key holds a credential.
func IsSecretConfigKey(k string) bool {
	_, ok := secretConfigKeys[normalizeConfigKey(k)]
	return ok
}

func normalizeConfigKey(k string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(k), "-", "_"))
}

// isSecretHeader reports whether an HTTP header name carries a credential.
func isSecretHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	switch n {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
	for _, part := range []string{"token", "secret", "password", "api-key", "apikey", "api_key", "auth", "session", "signature"} {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

// RedactSecretConfig returns a deep copy of cfg with every credential value
// replaced by SecretConfigPlaceholder: secret keys at any depth, credential
// headers inside a "headers" map, and the password in any URL userinfo.
// Keys listed in reveal (top level only) are copied unchanged.
func RedactSecretConfig(cfg map[string]any, reveal ...string) map[string]any {
	if cfg == nil {
		return nil
	}
	keep := make(map[string]struct{}, len(reveal))
	for _, k := range reveal {
		keep[normalizeConfigKey(k)] = struct{}{}
	}
	return redactMap(cfg, keep, false)
}

func redactMap(in map[string]any, keep map[string]struct{}, headerMap bool) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		if _, ok := keep[normalizeConfigKey(k)]; ok {
			out[k] = v
			continue
		}
		secret := IsSecretConfigKey(k) || (headerMap && isSecretHeader(k))
		out[k] = redactValue(k, v, secret)
	}
	return out
}

func redactValue(key string, v any, secret bool) any {
	switch t := v.(type) {
	case string:
		if t == "" {
			return t
		}
		if secret {
			return SecretConfigPlaceholder
		}
		return RedactURLPassword(t)
	case map[string]any:
		return redactMap(t, nil, normalizeConfigKey(key) == "headers")
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(key, e, secret)
		}
		return out
	default:
		return v
	}
}

// RedactURLPassword masks the password in a URL's userinfo
// (scheme://user:pass@host). Anything that is not such a URL is returned
// unchanged.
func RedactURLPassword(s string) string {
	u, ok := parseURLWithPassword(s)
	if !ok {
		return s
	}
	u.User = url.UserPassword(u.User.Username(), SecretConfigPlaceholder)
	return u.String()
}

func parseURLWithPassword(s string) (*url.URL, bool) {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return nil, false
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return nil, false
	}
	if _, has := u.User.Password(); !has {
		return nil, false
	}
	return u, true
}

// RestoreSecretConfig merges an update request with the stored config: any
// value equal to SecretConfigPlaceholder (or a URL whose password is the
// placeholder) is replaced by the stored value at the same path. A
// placeholder with nothing stored behind it is dropped, so the marker is
// never persisted. Every other incoming value, including a new secret,
// wins.
func RestoreSecretConfig(stored, incoming map[string]any) map[string]any {
	if incoming == nil {
		return nil
	}
	return restoreMap(stored, incoming)
}

func restoreMap(stored, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(incoming))
	for k, v := range incoming {
		prev, had := stored[k]
		restored, keep := restoreValue(prev, had, v)
		if keep {
			out[k] = restored
		}
	}
	return out
}

func restoreValue(prev any, had bool, v any) (any, bool) {
	switch t := v.(type) {
	case string:
		if t == SecretConfigPlaceholder {
			return prev, had
		}
		if u, ok := parseURLWithPassword(t); ok {
			if p, _ := u.User.Password(); p == SecretConfigPlaceholder {
				prevStr, _ := prev.(string)
				pu, ok := parseURLWithPassword(prevStr)
				if !ok {
					u.User = url.User(u.User.Username())
					return u.String(), true
				}
				pp, _ := pu.User.Password()
				u.User = url.UserPassword(u.User.Username(), pp)
				return u.String(), true
			}
		}
		return t, true
	case map[string]any:
		pm, _ := prev.(map[string]any)
		return restoreMap(pm, t), true
	case []any:
		pl, _ := prev.([]any)
		out := make([]any, 0, len(t))
		for i, e := range t {
			var pe any
			has := i < len(pl)
			if has {
				pe = pl[i]
			}
			if r, keep := restoreValue(pe, has, e); keep {
				out = append(out, r)
			}
		}
		return out, true
	default:
		return v, true
	}
}

// ConfigHasSecretPlaceholder reports whether cfg carries the placeholder
// anywhere. Create requests have no stored value to restore, so a
// placeholder there is a client error rather than a secret to persist.
func ConfigHasSecretPlaceholder(cfg map[string]any) bool {
	for _, v := range cfg {
		if valueHasPlaceholder(v) {
			return true
		}
	}
	return false
}

func valueHasPlaceholder(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, SecretConfigPlaceholder)
	case map[string]any:
		return ConfigHasSecretPlaceholder(t)
	case []any:
		for _, e := range t {
			if valueHasPlaceholder(e) {
				return true
			}
		}
	}
	return false
}

// notificationSecretKeys are keys that are credentials on a notification
// config but not on a monitor: a notification's "url" is the delivery
// endpoint (a generic webhook URL usually embeds its token), while a
// monitor's "url" is the public target it checks.
var notificationSecretKeys = []string{"url"}

// RedactNotificationConfig is RedactSecretConfig plus the notification-only
// credential keys. RestoreSecretConfig undoes it like any other placeholder.
func RedactNotificationConfig(cfg map[string]any) map[string]any {
	out := RedactSecretConfig(cfg)
	for _, k := range notificationSecretKeys {
		if v, ok := out[k].(string); ok && v != "" {
			out[k] = SecretConfigPlaceholder
		}
	}
	return out
}
