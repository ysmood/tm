package proto_test

import (
	"bytes"
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/proto"
)

func TestFrameRoundTrip(t *testing.T) {
	g := got.T(t)

	check := func(mt proto.MsgType, payload []byte) {
		g.Helper()

		var buf bytes.Buffer

		g.E(proto.WriteFrame(&buf, mt, payload))
		gotType, gotPayload, err := proto.ReadFrame(&buf)
		g.E(err)
		g.Eq(gotType, mt)
		g.Eq(gotPayload, payload)
	}

	check(proto.MsgInput, []byte("hello"))
	check(proto.MsgDetach, nil)
	check(proto.MsgOutput, bytes.Repeat([]byte("x"), 70000))
}

func TestAttachRoundTrip(t *testing.T) {
	g := got.T(t)
	in := proto.Attach{Hist: proto.HistLines, Lines: 42, Cols: 120, Rows: 40}
	out, err := proto.DecodeAttach(in.Encode())
	g.E(err)
	g.Eq(out, in)
}

func TestResizeRoundTrip(t *testing.T) {
	g := got.T(t)
	in := proto.Resize{Cols: 200, Rows: 50}
	out, err := proto.DecodeResize(in.Encode())
	g.E(err)
	g.Eq(out, in)
}

func TestExitRoundTrip(t *testing.T) {
	g := got.T(t)
	out, err := proto.DecodeExit(proto.EncodeExit(-7))
	g.E(err)
	g.Eq(out, int32(-7))
}

func TestSwitchTargetRoundTrip(t *testing.T) {
	g := got.T(t)

	check := func(in proto.SwitchTarget) {
		g.Helper()

		out, err := proto.DecodeSwitchTarget(in.Encode())
		g.E(err)
		g.Eq(out, in)
	}

	check(proto.SwitchTarget{ID: "abc123", Name: "my session", Hist: proto.HistAll, Lines: 7})
	check(proto.SwitchTarget{ID: "abc123", Name: "", Hist: proto.HistNone, Lines: 0}) // no name
	check(proto.SwitchTarget{ID: "", Name: "orphan", Hist: proto.HistPage, Lines: 0}) // no id
}

func TestDecodeShortPayloads(t *testing.T) {
	g := got.T(t)
	_, err := proto.DecodeAttach([]byte{1, 2})
	g.Err(err)
	_, err = proto.DecodeResize([]byte{1})
	g.Err(err)
	_, err = proto.DecodeExit(nil)
	g.Err(err)
	_, err = proto.DecodeSwitchTarget([]byte{1, 2, 3, 4, 5}) // shorter than the 7-byte header
	g.Err(err)
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	g := got.T(t)

	var buf bytes.Buffer

	err := proto.WriteFrame(&buf, proto.MsgOutput, make([]byte, proto.MaxPayload+1))
	g.Err(err)
}
