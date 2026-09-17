package history

import (
	"bytes"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing/format/packfile"
)

func TestPatchBlobDelta(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		target string
	}{
		{name: "insert", source: "base", target: "replacement"},
		{name: "copy and insert", source: "hello world", target: "hello brave world"},
		{name: "large copy", source: strings.Repeat("abcdefgh", 32<<10), target: strings.Repeat("abcdefgh", 32<<10) + "tail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			delta := packfile.DiffDelta([]byte(test.source), []byte(test.target))
			got, err := patchBlobDelta([]byte(test.source), delta)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(test.target)) {
				t.Fatalf("patched %d bytes, want %d", len(got), len(test.target))
			}
		})
	}
}

func TestPatchBlobDeltaRejectsMalformedInput(t *testing.T) {
	for _, delta := range [][]byte{
		nil,
		{4, 1, 1, 'x'},
		{0, 1, 0},
		{0, 1, 0x80},
		{0, 1, 2, 'x'},
		{0, 1, 0x8f, 0xff, 0xff, 0xff, 0xff},
	} {
		if _, err := patchBlobDelta(nil, delta); err == nil {
			t.Fatalf("patchBlobDelta accepted %x", delta)
		}
	}
}
