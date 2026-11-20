package strictjson_test

import (
	"strings"
	"testing"

	"berth/internal/proxyctl"
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

type embeds struct {
	X map[string]int `json:"x"`
	embedded
}

type deepEmbeds struct {
	List []*struct {
		embedded
	} `json:"list"`
}

type tree struct {
	Kids []*tree `json:"kids"`
}

// Every type Berth decodes with strictjson today decodes; native's outcome and berth-mcp's
// argument structs are local to their packages, whose tests decode them. A type that embeds a
// struct anywhere is refused before any byte is read, and map keys repeat when they decode
// alike.
func TestDecodeTakesPlainStructsAndRefusesEmbedding(t *testing.T) {
	for in, into := range map[string]any{
		`{"secrets":["gh-token"],"mode":"agent"}`:                                              &proxyctl.Request{},
		`{"port":1,"credential":"c","ca_file":"a","bundle_file":"b","placeholders":{"K":"v"}}`: &proxyctl.Control{},
		`{}`:                     &struct{}{},
		`{"kids":[{"kids":[]}]}`: &tree{},
	} {
		if err := strictjson.Decode([]byte(in), into); err != nil {
			t.Errorf("%s into %T: %v", in, into, err)
		}
	}
	for _, into := range []any{&embeds{}, &deepEmbeds{}} {
		if err := strictjson.Decode([]byte(`{}`), into); err == nil || !strings.Contains(err.Error(), "embeds") {
			t.Errorf("%T: %v, want refused for embedding", into, err)
		}
	}
	for in, ok := range map[string]bool{`{"1":"a","2":"b"}`: true, `{"1":"a","01":"b"}`: false, `{"1":"a","+1":"b"}`: false} {
		var m map[int]string
		if err := strictjson.Decode([]byte(in), &m); (err == nil) != ok {
			t.Errorf("%s into map[int]string: %v, want ok %v", in, err, ok)
		}
	}
}
