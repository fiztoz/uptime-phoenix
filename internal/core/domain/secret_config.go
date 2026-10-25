package domain

import (
	"encoding/json"
	"net/url"
	"reflect"
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
	// Push monitor signature key (handlers/push.go verifies X-Signature with it).
	"hmac_secret": {}, "hmacsecret": {},
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
		if secret || (normalizeConfigKey(key) == "headers" && headersStringHasSecret(t)) {
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

// headersStringHasSecret reports whether a "headers" value saved as a JSON
// string (the notification form's textarea does this) carries a credential
// header. Such a string is masked as a whole: its parts cannot be restored
// individually. Text that is not a JSON object cannot be inspected, so it is
// treated as secret.
func headersStringHasSecret(s string) bool {
	var hdrs map[string]any
	if err := json.Unmarshal([]byte(s), &hdrs); err != nil {
		return true
	}
	for name, v := range hdrs {
		if isSecretHeader(name) {
			return true
		}
		if str, ok := v.(string); ok && RedactURLPassword(str) != str {
			return true
		}
	}
	return false
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
//
// Inside a list, an element carrying a placeholder is restored from the one
// stored element whose masked form it equals, or else from the same index
// when the list kept its length. When neither identifies the stored element
// (an element was added or removed and the masked forms are ambiguous) the
// placeholder is left in place rather than guessed: restoring by index would
// hand one entry's secret to another. Callers must refuse a result for
// which ConfigHasSecretPlaceholder is true.
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
		restored, keep := restoreValue(k, prev, had, v)
		if keep {
			out[k] = restored
		}
	}
	return out
}

// restoreList restores the placeholders in a list value. See
// RestoreSecretConfig for the matching rules.
func restoreList(key string, prev []any, incoming []any) []any {
	out := make([]any, 0, len(incoming))
	for i, e := range incoming {
		if !valueHasPlaceholder(e) {
			out = append(out, e)
			continue
		}
		pe, found := uniqueMaskedMatch(key, prev, e)
		if !found && len(prev) == len(incoming) {
			pe, found = prev[i], true
		}
		if !found {
			out = append(out, e) // unresolvable: the caller refuses it
			continue
		}
		if r, keep := restoreValue(key, pe, true, e); keep {
			out = append(out, r)
		}
	}
	return out
}

// uniqueMaskedMatch returns the single stored element whose masked form is
// exactly the incoming element.
func uniqueMaskedMatch(key string, prev []any, e any) (any, bool) {
	var match any
	n := 0
	for _, pe := range prev {
		if reflect.DeepEqual(redactValue(key, pe, IsSecretConfigKey(key)), e) {
			match = pe
			n++
		}
	}
	return match, n == 1
}

func restoreValue(key string, prev any, had bool, v any) (any, bool) {
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
		return restoreList(key, pl, t), true
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
