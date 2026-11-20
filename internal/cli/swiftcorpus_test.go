package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	berthv1 "berth/gen/berth/v1"
	"berth/gen/berth/v1/berthv1connect"
)

// The generated corpora are the differential test between Go and Berth.app: Go writes what
// shown, quoted and connect-go's stream reader make of every entry, and the Swift tests assert
// that the app's twins make the same. Each corpus is a hand list plus entries drawn from a fixed
// seed, so it changes only when the code does, and TestSwiftCorpus fails when the checked in
// files no longer match.
const corpusSeed = 20261119

// textEntry is one input to shown and quoted. In is the input's bytes in hex, repeated Repeat
// times; Valid says whether it is UTF-8, which a Swift String must be. Long outputs are given by
// their SHA-256.
type textEntry struct {
	In           string `json:"in"`
	Repeat       int    `json:"repeat"`
	Valid        bool   `json:"valid"`
	Out          string `json:"out"`
	Quoted       string `json:"quoted"`
	OutSHA256    string `json:"outSHA256,omitempty"`
	QuotedSHA256 string `json:"quotedSHA256,omitempty"`
}

// handText are the inputs the corpus always holds: C0 and C1 controls, bidi controls, combining
// marks first and inside, encoded surrogates and other invalid UTF-8, and lengths far past any
// real name.
var handText = []struct {
	in     string
	repeat int
}{
	{"\x00\x01\x1b\x1f\x7f", 1},
	{"\u0080\u0085\u009b\u009f", 1},
	{"\u202a\u202b\u202c\u202d\u202e", 1},
	{"\u2066\u2067\u2068\u2069\u200e\u200f\u061c", 1},
	{"\u0301", 1},
	{"\u0301\u0302\u20dd", 1},
	{"a\u0301", 1},
	{"\ufe0f", 1},
	{"\U000e0100x", 1},
	{"\u20dd", 1},
	{"\u0903", 1},
	{"\xed\xa0\x80", 1},
	{"\xed\xbf\xbf", 1},
	{"\xed\xa0\xbd\xed\xb8\x80", 1},
	{"\xff", 1},
	{"\xc0\xaf", 1},
	{"\xe0\x80\xaf", 1},
	{"\xf4\x90\x80\x80", 1},
	{"a\xc3", 1},
	{"\xc3(", 1},
	{"\U0010ffff", 1},
	{"\uffff\ufffe\ufdd0", 1},
	{"\ufffd", 1},
	{"\u00ad\u200b\ufeff", 1},
	{"\u2028\u2029", 1},
	{"\ue000\U000f0000\U00100000", 1},
	{`"x\n"`, 1},
	{`"`, 1},
	{`\`, 1},
	{"\U0001f600\u200d\U0001f600", 1},
	{"\"\u0301x", 1},
	{"\"\ufe0fx", 1},
	{"\n\u0301", 1},
	{"\\\u0301", 1},
	{"\"\u0301", 1},
	{"\x1b\u0301\u20dd", 1},
	{"\xff\u0301", 1},
	{"\u0301\u0301", 1},
	{"\U000e0100\U000e0101", 1},
	{"a", 1 << 20},
	{"\x1b", 1 << 16},
	{"\u0301", 1 << 16},
	{"\xff", 1 << 16},
	{"\u202e", 1 << 16},
	{"ab\u0301", 1 << 15},
}

// textPool is what generated entries draw from, by class. Every code point here has had the same
// general category in every Unicode version since 6.3, so Go's tables and the app's agree.
var textPool = [][]rune{
	[]rune("abcXYZ019 ~-_./\"\\'`{}"),
	{0x00, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x1b, 0x1f, 0x7f},
	{0x80, 0x85, 0x8d, 0x9b, 0x9c, 0x9f},
	{0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069, 0x061c},
	{0x0300, 0x0301, 0x0327, 0x036f, 0x0903, 0x093f, 0x20dd, 0x20e3, 0xfe00, 0xfe0f, 0xe0100, 0xe01ef},
	{0x00ad, 0x200b, 0x200c, 0x200d, 0x2060, 0xfeff, 0xe0001, 0xe007f},
	{0x2028, 0x2029, 0xe000, 0xf8ff, 0xf0000, 0x10fffd, 0xfffe, 0xffff, 0xfdd0},
	{0x00e9, 0x00a0, 0x3000, 0x65e5, 0x672c, 0x1f600, 0x1f1fa, 0x0627, 0x05d0, 0xfffd},
}

func textCorpus() []textEntry {
	r := rand.New(rand.NewPCG(corpusSeed, 1))
	inputs := handText
	for range 400 {
		var b []byte
		for range r.IntN(12) {
			if r.IntN(10) == 0 {
				// A raw byte: a lone continuation, a lead without its tail, or an encoded surrogate.
				b = append(b, []byte{0x80, 0xbf, 0xc2, 0xe2, 0xf0, 0xff, 0xed, 0xa0}[r.IntN(8)])
				continue
			}
			class := textPool[r.IntN(len(textPool))]
			b = utf8.AppendRune(b, class[r.IntN(len(class))])
		}
		inputs = append(inputs, struct {
			in     string
			repeat int
		}{string(b), 1})
	}
	out := make([]textEntry, 0, len(inputs))
	for _, c := range inputs {
		text := strings.Repeat(c.in, c.repeat)
		e := textEntry{In: hex.EncodeToString([]byte(c.in)), Repeat: c.repeat, Valid: utf8.ValidString(text)}
		if c.repeat == 1 {
			e.Out, e.Quoted = shown(text), quoted(text)
		} else {
			e.OutSHA256, e.QuotedSHA256 = sum(shown(text)), sum(quoted(text))
		}
		out = append(out, e)
	}
	return out
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// envelopeEntry is one stream body: the bytes in hex, the sizes the reads return, and what
// connect-go makes of it: the messages it yields, then "ok" for a clean end, "refused <code>:
// <message>" for an error berthd sent, or "malformed".
type envelopeEntry struct {
	Name     string   `json:"name"`
	Body     string   `json:"body"`
	Cuts     []int    `json:"cuts"`
	Messages []string `json:"messages"`
	Outcome  string   `json:"outcome"`
}

// refusal is the error the corpus plants in end of stream messages, so an outcome can tell an
// error berthd sent from one the reader raised.
const refusal = "berth says no"

func frame(flags byte, payload string) []byte {
	b := []byte{flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	return append(b, payload...)
}

// lying is a frame whose length field says n whatever the payload is.
func lying(flags byte, n uint32, payload string) []byte {
	b := frame(flags, payload)
	binary.BigEndian.PutUint32(b[1:], n)
	return b
}

var (
	endOK      = frame(2, `{}`)
	endRefused = frame(2, `{"error":{"code":"permission_denied","message":"`+refusal+`"}}`)
)

func snapshotMsg(id int) []byte {
	return frame(0, fmt.Sprintf(`{"snapshot":{"eventId":"%d","sessions":[{"id":"1-2","state":"SESSION_STATE_IDLE"}]}}`, id))
}

func changeMsg(id int) []byte {
	return frame(0, fmt.Sprintf(`{"change":{"eventId":"%d","event":"Busy","session":{"id":"1-2"}}}`, id))
}

func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

var handEnvelopes = []struct {
	name string
	body []byte
}{
	{"empty body", nil},
	{"only a clean end", endOK},
	{"two messages and a clean end", join(snapshotMsg(10), changeMsg(12), endOK)},
	{"messages then a refusal", join(snapshotMsg(10), endRefused)},
	{"no end of stream", join(snapshotMsg(10), changeMsg(12))},
	{"end with metadata", join(changeMsg(1), frame(2, `{"metadata":{"x-a":["b"]}}`))},
	{"end not JSON", join(changeMsg(1), frame(2, `{"error":`))},
	{"end an array", frame(2, `[]`)},
	{"end empty", frame(2, ``)},
	{"end error with an unknown code", frame(2, `{"error":{"code":"bogus","message":"`+refusal+`"}}`)},
	{"end error without a code", frame(2, `{"error":{"message":"`+refusal+`"}}`)},
	{"message not JSON", join(frame(0, `{"snapshot":`), endOK)},
	{"message null", join(frame(0, `null`), endOK)},
	{"message empty object", join(frame(0, `{}`), endOK)},
	{"message empty", join(frame(0, ``), endOK)},
	{"compressed message", join(frame(1, `{}`), endOK)},
	{"compressed end", frame(3, `{}`)},
	{"unknown flag on a message", join(frame(4, `{}`), endOK)},
	{"unknown flag on the end", frame(6, `{}`)},
	{"high flag on a message", join(frame(0x80, `{}`), endOK)},
	{"high flag on the end", frame(0x82, `{}`)},
	{"data after the end", join(endOK, snapshotMsg(5))},
	{"garbage after the end", join(endOK, []byte("junk"))},
	{"two ends", join(endOK, endRefused)},
	{"length past the data", join(snapshotMsg(1), lying(0, 100, `{}`))},
	{"length short of the data", join(lying(0, 1, `{}`), endOK)},
	{"length the largest", lying(0, 0xffffffff, ``)},
	{"length one past 16 MiB", lying(0, 16<<20+1, ``)},
	{"length 16 MiB with no data", lying(0, 16<<20, ``)},
	{"end length past the data", lying(2, 1000, `{}`)},
	{"half a header", []byte{0, 0, 0}},
	{"message then half a header", join(snapshotMsg(3), []byte{2, 0})},
}

func envelopeCorpus(t *testing.T) []envelopeEntry {
	r := rand.New(rand.NewPCG(corpusSeed, 2))
	type item struct {
		name string
		body []byte
	}
	items := make([]item, 0, len(handEnvelopes)+150)
	for _, h := range handEnvelopes {
		items = append(items, item{h.name, h.body})
	}
	pieces := []func(int) []byte{
		snapshotMsg, changeMsg,
		func(int) []byte { return endOK },
		func(int) []byte { return endRefused },
		func(int) []byte { return frame(2, `{"error":`) },
		func(int) []byte { return frame(0, `{"change":`) },
		func(int) []byte { return frame([]byte{1, 3, 4, 6, 0x80}[r.IntN(5)], `{}`) },
		func(n int) []byte { return lying(0, uint32(n), `{}`) },
		func(int) []byte { return []byte{byte(r.Uint32())} },
	}
	for i := range 150 {
		var body []byte
		for range r.IntN(5) {
			p := pieces[r.IntN(len(pieces))]
			if r.IntN(3) > 0 {
				p = pieces[r.IntN(2)]
			}
			body = append(body, p(r.IntN(40))...)
		}
		if r.IntN(2) == 0 {
			body = append(body, endOK...)
		}
		items = append(items, item{fmt.Sprintf("generated %d", i), body})
	}
	out := make([]envelopeEntry, 0, len(items))
	for _, it := range items {
		cuts := []int{1 + r.IntN(64)}
		for range r.IntN(4) {
			cuts = append(cuts, 1+r.IntN(16))
		}
		msgs, outcome := goReads(t, it.body, cuts)
		out = append(out, envelopeEntry{Name: it.name, Body: hex.EncodeToString(it.body), Cuts: cuts, Messages: msgs, Outcome: outcome})
	}
	return out
}

// cutReader returns body in reads of the given sizes, the last size repeating.
type cutReader struct {
	body []byte
	cuts []int
}

func (c *cutReader) Read(p []byte) (int, error) {
	if len(c.body) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), len(c.body), c.cuts[0])
	if len(c.cuts) > 1 {
		c.cuts = c.cuts[1:]
	}
	copy(p, c.body[:n])
	c.body = c.body[n:]
	return n, nil
}

// goReads runs body through connect-go's own client, as the berth CLI reads a stream.
func goReads(t *testing.T, body []byte, cuts []int) ([]string, string) {
	t.Helper()
	answer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			go func() { _, _ = io.Copy(io.Discard, req.Body) }()
		}
		return &http.Response{
			StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": {"application/connect+json"}}, ContentLength: -1,
			Body: io.NopCloser(&cutReader{body: body, cuts: cuts}), Request: req,
		}, nil
	})
	c := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: answer}, "http://berthd", connecthttp.WithProtoJSON()))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream, err := berthv1connect.NewSessionsServiceClient(c).WatchSessions(ctx, &berthv1.WatchSessionsRequest{})
	if err != nil {
		return []string{}, outcomeOf(err)
	}
	defer func() { _ = stream.Close() }()
	msgs := []string{}
	for {
		m, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return msgs, "ok"
		}
		if err != nil {
			return msgs, outcomeOf(err)
		}
		switch {
		case m.GetSnapshot() != nil:
			msgs = append(msgs, fmt.Sprintf("snapshot %d", m.GetSnapshot().GetEventId()))
		case m.GetChange() != nil:
			msgs = append(msgs, fmt.Sprintf("change %d", m.GetChange().GetEventId()))
		default:
			msgs = append(msgs, "unknown")
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func outcomeOf(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) && ce.Message() == refusal {
		return "refused " + ce.Code().String()
	}
	return "malformed"
}

func TestSwiftCorpus(t *testing.T) {
	text := textCorpus()
	var quotedLead, escaped, plain, invalid bool
	for _, e := range text {
		quotedLead = quotedLead || strings.HasPrefix(e.Quoted, `"\u0`)
		escaped = escaped || (e.Valid && e.Out != "" && e.Out[0] == '"')
		plain = plain || (e.Valid && e.Repeat == 1 && e.Out != "" && e.Out[0] != '"')
		invalid = invalid || !e.Valid
	}
	if !quotedLead || !escaped || !plain || !invalid || !unicode.IsGraphic('a') {
		t.Fatal("the text corpus no longer reaches every branch of shown and quoted")
	}
	corpusFile(t, "text_corpus.json", text)
	envelopes := envelopeCorpus(t)
	outcomes := map[string]bool{}
	for _, e := range envelopes {
		outcomes[strings.Fields(e.Outcome)[0]] = true
	}
	if !outcomes["ok"] || !outcomes["refused"] || !outcomes["malformed"] {
		t.Fatalf("the envelope corpus reaches only %v", outcomes)
	}
	corpusFile(t, "envelopes.json", envelopes)
}

// corpusFile writes want under -update-swift, and otherwise fails unless the checked in file
// holds exactly want.
func corpusFile[T any](t *testing.T, name string, want []T) {
	t.Helper()
	path := filepath.Join(swiftFixtures, name)
	if *updateSwift {
		writeJSON(t, path, want)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(want); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run go test ./internal/cli -run TestSwift -update-swift", err)
	}
	if !bytes.Equal(got, buf.Bytes()) {
		t.Fatalf("%s is stale; run go test ./internal/cli -run TestSwift -update-swift", path)
	}
}
