package proto_test

import (
	"testing"

	"github.com/ysmood/got"
	"github.com/ysmood/tm/pkg/proto"
)

func TestAttachRoundTrip(t *testing.T) {
	g := got.T(t)
	in := proto.Attach{Replay: true, Cols: 120, Rows: 40}
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

func TestSwitchTargetRoundTrip(t *testing.T) {
	g := got.T(t)

	check := func(in proto.SwitchTarget) {
		g.Helper()

		out, err := proto.DecodeSwitchTarget(in.Encode())
		g.E(err)
		g.Eq(out, in)
	}

	check(proto.SwitchTarget{ID: "abc123", Name: "my session"})
	check(proto.SwitchTarget{ID: "abc123", Name: ""}) // no name
	check(proto.SwitchTarget{ID: "", Name: "orphan"}) // no id
}

func TestDecodeShortPayloads(t *testing.T) {
	g := got.T(t)
	_, err := proto.DecodeAttach([]byte{1, 2})
	g.Err(err)
	_, err = proto.DecodeResize([]byte{1})
	g.Err(err)
	_, err = proto.DecodeSwitchTarget([]byte{1}) // shorter than the 2-byte id-length header
	g.Err(err)
	_, err = proto.DecodeSwitchTarget([]byte{0, 9, 'a'}) // header promises 9 id bytes, 1 follows
	g.Err(err)
}
