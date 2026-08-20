// Package event — payload codec (P1, card #148).
//
// The codec serializes and deserializes event payloads for the SQLite event
// log. P1 uses JSON (encoding "json-v1"); P2 will introduce protobuf
// ("proto-v1"). The events table carries an `encoding` column so the reader can
// dispatch on the encoding format.
//
// The codec is built on the existing payloadTypes registry (kinds.go): Kind →
// reflect.Type. MarshalPayload validates that the payload's concrete type
// matches the registered type for the kind, then JSON-encodes. UnmarshalPayload
// creates a new zero value of the registered type, JSON-decodes into it, and
// validates the result. Both functions return the payload as a VALUE (not a
// pointer), matching the in-memory representation that MemLog preserves.

package event

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// PayloadEncoding is the encoding format identifier stored in the events table.
// P1 uses "json-v1"; P2 will add "proto-v1". The reader dispatches on this
// value; unknown encodings are rejected.
const PayloadEncoding = "json-v1"

// MarshalPayload validates that payload's concrete type matches the registered
// type for kind, then JSON-encodes it. It rejects nil payloads, typed-nil
// pointers, mismatched types, and non-encodable values (NaN, Inf).
func MarshalPayload(kind Kind, payload any) ([]byte, error) {
	if err := validatePayloadType(kind, payload); err != nil {
		return nil, err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("event %s: marshal payload: %w", kind, err)
	}
	return data, nil
}

// UnmarshalPayload creates a new zero value of the registered concrete type for
// kind, JSON-decodes data into it, validates the result, and returns the
// payload as a VALUE (not a pointer). It rejects unknown kinds, malformed JSON,
// validation failures, and payloads that decode into invalid states.
func UnmarshalPayload(kind Kind, data []byte) (any, error) {
	t, ok := payloadTypes[kind]
	if !ok {
		return nil, fmt.Errorf("event: unknown kind %q for unmarshal", kind)
	}
	// Create a pointer to a new zero value of the concrete type, decode into it.
	ptr := reflect.New(t)
	if err := json.Unmarshal(data, ptr.Interface()); err != nil {
		return nil, fmt.Errorf("event %s: unmarshal payload: %w", kind, err)
	}
	payload := ptr.Elem().Interface()
	// Validate the decoded payload if it implements Validate().
	if v, ok := payload.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, fmt.Errorf("event %s: decoded payload invalid: %w", kind, err)
		}
	}
	return payload, nil
}
