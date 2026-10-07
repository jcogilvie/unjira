package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/jcogilvie/unjira/internal/events"
)

// TestBlankMarkdownCode_KeysQuotedAsCodeAreNotProse is the shape a real 30-day store's
// pull request bodies took: every key that became a wrong primary from a PR body sat in
// a code span or a fenced block, quoted as an example, a crash message or test output.
func TestBlankMarkdownCode_KeysQuotedAsCodeAreNotProse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "inline code span",
			body: "neither can match `PAAS-4001`, so it stays unlinked",
			want: nil,
		},
		{
			name: "double-backtick span holding a backtick",
			body: "the parser saw ``PAAS-7 ` quoted`` here",
			want: nil,
		},
		{
			name: "fenced block of test output",
			body: "A drain pass aborted:\n\n```\nadding issue link PAAS-3899 (primary)\n```\n\nFixed.",
			want: nil,
		},
		{
			name: "tilde fence",
			body: "~~~text\nprose keys: [\"SIA-201\"]\n~~~\n",
			want: nil,
		},
		{
			name: "indented code block",
			body: "Output:\n\n    L310-326 PAAS-9\n\nend",
			want: nil,
		},
		{
			name: "fence inside a blockquote",
			body: "> ```\n> DEVSBX-517\n> ```\n",
			want: nil,
		},
		{
			name: "a key named in prose survives beside a quoted one",
			body: "Closes PAAS-123. The old `PAAS-9` example is gone.",
			want: []string{"PAAS-123"},
		},
		{
			name: "a list continuation is prose, not an indented code block",
			body: "- ParseIssueKey reads a scope:\n  PAAS-123 -> PAAS\n",
			want: []string{"PAAS-123"},
		},
		{
			name: "a link destination is prose",
			body: "Tracked in [the ticket](https://example.atlassian.net/browse/PAAS-55).",
			want: []string{"PAAS-55"},
		},
		{
			name: "an unclosed backtick is literal text, not a code span",
			body: "a stray ` then PAAS-77 named in prose",
			want: []string{"PAAS-77"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := events.ExtractTicketKeys(events.BlankMarkdownCode(tc.body))

			assert.Equal(t, tc.want, got, "keys outside code in %q", tc.body)
		})
	}
}

// TestBlankMarkdownCode_PreservesEveryByteOutsideCode pins that blanking keeps the
// text's length and every byte outside code, so two words either side of a code span
// cannot be joined into one token and nothing outside code is lost.
func TestBlankMarkdownCode_PreservesEveryByteOutsideCode(t *testing.T) {
	body := "A`PAAS-1`B and\n```\nX-1\n```\nend"

	got := events.BlankMarkdownCode(body)

	assert.Len(t, got, len(body))
	assert.Equal(t, "A`      `B and\n```\n   \n```\nend", got)
}
