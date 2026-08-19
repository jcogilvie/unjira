package config

import (
	"fmt"
	"strings"
	"time"

	str2duration "github.com/xhit/go-str2duration/v2"
)

// Span is a duration that also accepts day ("7d") and week ("2w") units,
// including compound forms ("7d12h"), which time.ParseDuration rejects — it
// stops at "h", deliberately, since a calendar day is not reliably 24h under
// DST. unjira measures spans backward from now in UTC, where a day is exactly
// 24h, so that ambiguity cannot arise here.
//
// Parsing is delegated to github.com/xhit/go-str2duration rather than
// hand-rolled: it handles compound forms a strip-the-suffix approach cannot.
// This type exists to satisfy encoding.TextUnmarshaler (honored by both Kong
// and encoding/json) and to reject non-positive spans.
type Span time.Duration

// UnmarshalText parses a duration string, accepting day and week units on top
// of everything time.ParseDuration handles. Non-positive spans are rejected:
// unjira's windows run backward from now, so a negative span would put the
// window's start after its end and silently produce an empty pass.
func (s *Span) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if raw == "" {
		return fmt.Errorf("parsing duration: empty value")
	}

	d, err := str2duration.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", raw, err)
	}

	if d <= 0 {
		return fmt.Errorf("parsing duration %q: must be positive", raw)
	}

	*s = Span(d)

	return nil
}

// Duration returns the span as a time.Duration. Pointer receiver to match
// UnmarshalText, which must be a pointer to satisfy encoding.TextUnmarshaler:
// a type mixing value and pointer receivers is both a lint finding
// (recvcheck) and a real hazard, since only the pointer method set is
// complete.
//
// Deliberately no String method. An earlier revision had one, but with a
// pointer receiver it does not participate in formatting a Span *value* —
// fmt.Printf("%s", someSpan) renders %!s(config.Span=604800000000000) rather
// than 168h0m0s, which is worse than having no method at all. Callers that
// want a human-readable form call Duration().String(), which always works.
func (s *Span) Duration() time.Duration {
	return time.Duration(*s)
}
