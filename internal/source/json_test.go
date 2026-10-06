package source

import (
	"reflect"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	got, err := DecodeJSON([]byte(`{"id":1700000000,"big":123456789012345678,"f":1.5,"nested":[{"n":2}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": int64(1700000000), "big": int64(123456789012345678), "f": float64(1.5), "nested": []any{map[string]any{"n": int64(2)}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
