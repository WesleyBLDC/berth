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

type embedded struct {
	X struct{ A int } `json:"x"`
}

type shallow struct {
	X map[string]int `json:"x"`
	embedded
}

type loop struct {
	*loop
	N int `json:"n"`
}

// A shallower field hides an embedded one of the same name, as in encoding/json, an embedding
// cycle ends, and map keys repeat when they decode alike.
func TestDecodeReadsFieldsAndKeysAsEncodingJSON(t *testing.T) {
	var s shallow
	if err := strictjson.Decode([]byte(`{"x":{"a":1}}`), &s); err != nil || s.X["a"] != 1 {
		t.Fatalf("the shallower map field: %v %+v", err, s)
	}
	var l loop
	if err := strictjson.Decode([]byte(`{"n":1}`), &l); err != nil || l.N != 1 || l.loop != nil {
		t.Fatalf("a self embedding struct: %v %+v", err, l)
	}
	for in, ok := range map[string]bool{`{"1":"a","2":"b"}`: true, `{"1":"a","01":"b"}`: false, `{"1":"a","+1":"b"}`: false} {
		var m map[int]string
		if err := strictjson.Decode([]byte(in), &m); (err == nil) != ok {
			t.Errorf("%s into map[int]string: %v, want ok %v", in, err, ok)
		}
	}
}
