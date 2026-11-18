package strictjson_test

import (
	"testing"

	"berth/internal/strictjson"
)

func TestDecodeTakesOneKnownValueAndNothingAfter(t *testing.T) {
	type line struct {
		A int `json:"a"`
	}
	for in, ok := range map[string]bool{
		`{"a":1}`:          true,
		"{\"a\":1}\n":      true,
		`{"a":1,"b":2}`:    false,
		`{"a":1}{"a":2}`:   false,
		`{"a":1}}`:         false,
		`{"a":1} junk`:     false,
		``:                 false,
		`{"a":"one"}`:      false,
		` {"a":1} ` + "\n": true,
	} {
		var got line
		if err := strictjson.Decode([]byte(in), &got); (err == nil) != ok {
			t.Errorf("%q: err %v, want ok %v", in, err, ok)
		}
	}
}
