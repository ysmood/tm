package proto

// The frame codec is unexported (Conn is the package's API), so its tests live
// inside the package.

import (
	"bytes"
	"testing"

	"github.com/ysmood/got"
)

func TestFrameRoundTrip(t *testing.T) {
	g := got.T(t)

	check := func(mt MsgType, payload []byte) {
		g.Helper()

		var buf bytes.Buffer

		g.E(writeFrame(&buf, mt, payload))
		gotType, gotPayload, err := readFrame(&buf)
		g.E(err)
		g.Eq(gotType, mt)
		g.Eq(gotPayload, payload)
	}

	check(MsgInput, []byte("hello"))
	check(MsgDetach, nil)
	check(MsgOutput, bytes.Repeat([]byte("x"), 70000)) // spans several reads
}

// A payload past MaxPayload is refused rather than written, so a corrupt length
// prefix can never be produced by this side.
func TestWriteFrameRejectsOversize(t *testing.T) {
	g := got.T(t)

	var buf bytes.Buffer

	g.Err(writeFrame(&buf, MsgOutput, make([]byte, MaxPayload+1)))
}
