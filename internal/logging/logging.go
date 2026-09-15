// Package logging builds the one logger unjira uses, from a level and a format.
//
// Added for finding F17: the pipeline's slowest stage said nothing while it ran, so a
// pass in progress was indistinguishable from a hung one, and there was no way to turn
// detail up when diagnosing or down when running `watch` on an interval. Logging was 35
// bare calls to the stdlib "log" package's formatted-print function, with no level and
// no structure.
//
// WHY log/slog rather than zap, zerolog, or logr. slog is stdlib as of Go 1.21 and this
// module is on 1.26, so it costs no dependency — and this module currently has no
// logging dependency at all, which is worth keeping. The usual argument for zap or
// zerolog is allocation-per-line throughput, and unjira's dominant cost is a
// multi-minute LLM call: paying an API surface for nanoseconds we cannot observe would
// be the wrong trade. logr is an interface for libraries that must not impose a backend,
// which is a library's problem and not a binary's — and slog now fills that niche in the
// stdlib regardless. slog.Handler remains the extension point if a backend is ever
// needed, so the decision is reversible at the handler rather than the call site.
//
// TEXT IS THE DEFAULT, JSON IS FIRST-CLASS. unjira is a CLI today, so the default has
// to be readable by a person at a terminal. It is also expected to run as a service
// across an org, and a structured mode retrofitted later means log consumers spend the
// interim parsing a human format with regexes. Both handlers emit the same attributes,
// so nothing is lost by switching.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Options is everything needed to build the logger. The zero value is usable: text at
// info level to stderr, which is what a CLI wants and what every test that does not care
// about logging should get for free.
type Options struct {
	// Level is one of debug, info, warn, error. Empty means info.
	Level string
	// Format is text or json. Empty means text.
	Format string
	// Out is where records go. Nil means os.Stderr — stderr rather than stdout because
	// unjira's stdout carries the rendered pass summary a human or a pipe consumes, and
	// interleaving diagnostics into it would corrupt both.
	Out io.Writer
}

// New builds a logger, or reports why it could not.
//
// An unrecognised level or format is an ERROR rather than a silent fall back to the
// default. A deployment that asked for JSON and got text would break at its log
// consumer, far from the typo that caused it; failing at startup names the bad value
// while the person who typed it is still watching.
func New(opts Options) (*slog.Logger, error) {
	level, err := parseLevel(opts.Level)
	if err != nil {
		return nil, err
	}

	out := opts.Out
	if out == nil {
		out = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{Level: level}

	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "", "text":
		return slog.New(slog.NewTextHandler(out, handlerOpts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(out, handlerOpts)), nil
	default:
		return nil, fmt.Errorf(
			"unknown log format %q: expected text or json", opts.Format)
	}
}

// Discard returns a logger that writes nowhere.
//
// The point is that library code can take a *slog.Logger and never nil-check it. A
// package handed no logger should behave identically to one handed a silent logger,
// which is what keeps `if log != nil` out of every call site.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// For tags a logger with the component emitting the record.
//
// This replaces the prefix convention every existing message carried —
// "pipeline: could not count...", "correlator: compacted narrative..." — which was an
// attribute wearing a costume: unqueryable once shipped as JSON, and repeated inside
// every format string where it could drift.
//
// Tolerates a nil logger and returns a working one, so a package can tag itself without
// first checking whether it was given anything.
func For(log *slog.Logger, component string) *slog.Logger {
	if log == nil {
		log = Discard()
	}

	return log.With("component", component)
}

// parseLevel maps a name to a slog level, rejecting anything else.
//
// Deliberately not accepting slog's own numeric levels or arbitrary abbreviations: the
// set is small enough to spell, and a closed set is what makes a typo an error instead
// of a surprise.
func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf(
			"unknown log level %q: expected debug, info, warn, or error", name)
	}
}
