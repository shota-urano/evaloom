package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
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
	var first error
	collectShape(data, typ, name, func(path, reason string) {
		if first == nil {
			first = fmt.Errorf("%s: %s", path, strings.ReplaceAll(reason, "_", " "))
		}
	})
	return first
}

// collectShape reports every shape problem instead of stopping at the first,
// so Profile registration can return all failing paths (spec 03 3.4).
// Paths are joined with "." and "[i]"; an empty name is the document root.
func collectShape(data []byte, typ reflect.Type, name string, report func(path, reason string)) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if typ.Kind() != reflect.Pointer {
			report(rootName(name), "must_not_be_null")
		}
		return
	}
	if typ.Kind() == reflect.Pointer {
		collectShape(data, typ.Elem(), name, report)
		return
	}
	if typ == rawMessageType {
		if err := objectJSON(data, name); err != nil {
			report(rootName(name), "expected_object")
		}
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		fields, err := objectFields(data, name)
		if err != nil {
			report(rootName(name), "expected_object")
			return
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			key := strings.Split(field.Tag.Get("json"), ",")[0]
			path := joinPath(name, key)
			raw, exists := fields[key]
			if !exists {
				if field.Tag.Get("required") == "true" {
					report(path, "required")
				}
				continue
			}
			delete(fields, key)
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && field.Tag.Get("nullable") != "true" {
				report(path, "must_not_be_null")
				continue
			}
			collectShape(raw, field.Type, path, report)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			report(joinPath(name, key), "unknown_field")
		}
	case reflect.Slice:
		var elements []json.RawMessage
		if err := json.Unmarshal(data, &elements); err != nil {
			report(rootName(name), "expected_array")
			return
		}
		for i, element := range elements {
			collectShape(element, typ.Elem(), fmt.Sprintf("%s[%d]", name, i), report)
		}
	case reflect.String:
		var value string
		if json.Unmarshal(data, &value) != nil {
			report(rootName(name), "expected_string")
		}
	case reflect.Bool:
		var value bool
		if json.Unmarshal(data, &value) != nil {
			report(rootName(name), "expected_boolean")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var value int64
		if json.Unmarshal(data, &value) != nil {
			report(rootName(name), "expected_integer")
		}
	case reflect.Float32, reflect.Float64:
		var value float64
		if json.Unmarshal(data, &value) != nil {
			report(rootName(name), "expected_number")
		}
	}
}

func joinPath(name, key string) string {
	if name == "" {
		return key
	}
	return name + "." + key
}

func rootName(name string) string {
	if name == "" {
		return "$"
	}
	return name
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
