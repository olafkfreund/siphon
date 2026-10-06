package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// DecodeJSON preserves JSON integers that fit in int64.
func DecodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return convertNumbers(value)
}

func convertNumbers(value any) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		return v.Float64()
	case map[string]any:
		for key, item := range v {
			converted, err := convertNumbers(item)
			if err != nil {
				return nil, err
			}
			v[key] = converted
		}
	case []any:
		for i, item := range v {
			converted, err := convertNumbers(item)
			if err != nil {
				return nil, err
			}
			v[i] = converted
		}
	}
	return value, nil
}
