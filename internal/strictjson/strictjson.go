// Package strictjson decodes JSON another program wrote for Berth to act on: exactly one value,
// no field Berth does not know and nothing after it, so no two readers can see different things.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Decode reads b into into, refusing unknown fields and anything but white space after the value.
func Decode(b []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON value")
	}
	return nil
}
