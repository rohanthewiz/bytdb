package bytdb

import (
	"errors"
	"testing"

	"github.com/rohanthewiz/serr"
)

func TestErrText(t *testing.T) {
	err := serr.New("wrong number of parameters", "want", "1", "got", "0")
	if got := ErrText(err); got != "wrong number of parameters (want: 1, got: 0)" {
		t.Fatalf("got %q", got)
	}

	// Attributes added across wraps come through; frame context and
	// wrap messages stay out.
	wrapped := serr.Wrap(err, "binding statement", "pos", "27")
	if got := ErrText(wrapped); got != "wrong number of parameters (want: 1, got: 0, pos: 27)" {
		t.Fatalf("got %q", got)
	}

	// Plain errors and serr errors without attributes pass through.
	if got := ErrText(errors.New("plain")); got != "plain" {
		t.Fatalf("got %q", got)
	}
	if got := ErrText(serr.New("bare message")); got != "bare message" {
		t.Fatalf("got %q", got)
	}
	if got := ErrText(nil); got != "" {
		t.Fatalf("got %q", got)
	}
}
