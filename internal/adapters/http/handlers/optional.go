package handlers

import "encoding/json"

// optionalNullableInt64 distinguishes a JSON key that was omitted from one
// that was sent as null. encoding/json only calls UnmarshalJSON when the key
// is present, so Set stays false on omit and true on both null and a number.
//
// Use this for clearable ID fields (group_id, proxy_id) on partial updates.
// A plain *int64 cannot tell "leave unchanged" from "clear".
type optionalNullableInt64 struct {
	set   bool
	value *int64
}

// UnmarshalJSON records that the field was present and stores either nil
// (JSON null) or the decoded integer.
func (o *optionalNullableInt64) UnmarshalJSON(data []byte) error {
	o.set = true
	var v *int64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	o.value = v
	return nil
}
