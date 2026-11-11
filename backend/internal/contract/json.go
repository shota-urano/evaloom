package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
)

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// Inspect the wire shape before decoding. DisallowUnknownFields alone accepts
// case aliases, null scalars and duplicate keys, all of which hide bad inputs.
// RawMessage is the deliberate boundary for harness-owned opaque objects.
func strictDecode(data []byte, dst any) error {
	if err := checkShape(data, reflect.TypeOf(dst).Elem(), "$"); err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

func checkShape(data []byte, typ reflect.Type, name string) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if typ.Kind() == reflect.Pointer {
			return nil
		}
		return fmt.Errorf("%s: must not be null", name)
	}
	if typ.Kind() == reflect.Pointer {
		return checkShape(data, typ.Elem(), name)
	}
	if typ == rawMessageType {
		return objectJSON(data, name)
	}
	switch typ.Kind() {
	case reflect.Struct:
		fields, err := objectFields(data, name)
		if err != nil {
			return err
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := strings.Split(field.Tag.Get("json"), ",")[0]
			raw, exists := fields[key]
			if !exists {
				if field.Tag.Get("required") == "true" {
					return fmt.Errorf("%s.%s: required field missing", name, key)
				}
				continue
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && field.Tag.Get("nullable") != "true" {
				return fmt.Errorf("%s.%s: must not be null", name, key)
			}
			if err := checkShape(raw, field.Type, name+"."+key); err != nil {
				return err
			}
			delete(fields, key)
		}
		for key := range fields {
			return fmt.Errorf("%s.%s: unknown field", name, key)
		}
	case reflect.Slice:
		var elements []json.RawMessage
		if err := json.Unmarshal(data, &elements); err != nil {
			return fmt.Errorf("%s: expected array", name)
		}
		for i, element := range elements {
			if err := checkShape(element, typ.Elem(), fmt.Sprintf("%s[%d]", name, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func objectFields(data []byte, name string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("%s: expected object", name)
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("%s: expected key", name)
		}
		if _, exists := fields[key]; exists {
			return nil, fmt.Errorf("%s.%s: duplicate field", name, key)
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s: trailing JSON", name)
	}
	return fields, nil
}

func objectJSON(data []byte, name string) error {
	_, err := objectFields(data, name)
	return err
}

func (r *Request) UnmarshalJSON(data []byte) error {
	type plain Request
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := Request(value).Validate(); err != nil {
		return err
	}
	*r = Request(value)
	return nil
}

func (r *Result) UnmarshalJSON(data []byte) error {
	type plain Result
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := Result(value).Validate(); err != nil {
		return err
	}
	*r = Result(value)
	return nil
}

func (f *Finding) UnmarshalJSON(data []byte) error {
	type plain Finding
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := Finding(value).Validate(); err != nil {
		return err
	}
	*f = Finding(value)
	return nil
}

func (p *ProfileDefinition) UnmarshalJSON(data []byte) error {
	type plain ProfileDefinition
	var value plain
	if err := strictDecode(data, &value); err != nil {
		return err
	}
	if err := ProfileDefinition(value).Validate(); err != nil {
		return err
	}
	*p = ProfileDefinition(value)
	return nil
}
