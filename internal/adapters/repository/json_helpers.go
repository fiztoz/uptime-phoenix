// Package repository provides shared Bun model structs, JSON helper types,
// and migration infrastructure for both MariaDB and SQLite adapters.
//
// These model structs map to database tables defined in the migration SQL files.
// Fields that reference security-related columns (e.g. password_hash, totp_secret,
// key_hash) are ORM column mappings — they hold database column names, not secret
// values themselves. Actual secret handling is done in the auth adapter layer.
package repository

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// JSON helper types — work with both MariaDB JSON columns and SQLite TEXT.
// ---------------------------------------------------------------------------

// JSONField maps a map[string]any to a JSON column. Implements sql.Scanner
// and driver.Valuer so Bun can marshal/unmarshal transparently.
type JSONField map[string]any

// Scan implements sql.Scanner.
func (j *JSONField) Scan(src any) error {
	if src == nil {
		*j = make(JSONField)
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("JSONField.Scan: unsupported type %T", src)
	}
	m := make(map[string]any)
	if err := json.Unmarshal(bytes, &m); err != nil {
		return fmt.Errorf("JSONField.Scan: unmarshal: %w", err)
	}
	*j = m
	return nil
}

// Value implements driver.Valuer.
func (j JSONField) Value() (driver.Value, error) {
	if j == nil {
		return "{}", nil
	}
	b, err := json.Marshal(j)
	if err != nil {
		return nil, fmt.Errorf("JSONField.Value: marshal: %w", err)
	}
	return string(b), nil
}

// ToMap converts JSONField to a plain map[string]any.
func (j JSONField) ToMap() map[string]any {
	if j == nil {
		return make(map[string]any)
	}
	return map[string]any(j)
}

// StringListField maps a []string to a JSON column. Implements sql.Scanner
// and driver.Valuer.
type StringListField []string

// Scan implements sql.Scanner.
func (s *StringListField) Scan(src any) error {
	if src == nil {
		*s = nil
		return nil
	}
	var bytes []byte
	switch v := src.(type) {
	case string:
		bytes = []byte(v)
	case []byte:
		bytes = v
	default:
		return fmt.Errorf("StringListField.Scan: unsupported type %T", src)
	}
	var arr []string
	if err := json.Unmarshal(bytes, &arr); err != nil {
		return fmt.Errorf("StringListField.Scan: unmarshal: %w", err)
	}
	*s = arr
	return nil
}

// Value implements driver.Valuer.
func (s StringListField) Value() (driver.Value, error) {
	if s == nil {
		return "[]", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("StringListField.Value: marshal: %w", err)
	}
	return string(b), nil
}
