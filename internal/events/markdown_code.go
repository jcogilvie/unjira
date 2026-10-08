package events

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// BlankMarkdownCode returns text with the content of every Markdown code region
// replaced by spaces: fenced and indented code blocks, and inline code spans. Every
// other byte, newlines inside code included, is kept at its position, so the result is
// as long as text, and words either side of a code span are never joined into a token.
//
// It exists so key extraction can tell a key the author wrote from a key the author
// quoted. Code is the author marking text as literal: a crash message, test output, a
// fixture, an example of key syntax. On a real 30-day store, every key that became a
// wrong primary from a pull request body was written that way: `PAAS-3905` named as the
// subject of a finding, PAAS-3899 inside a pasted constraint-failure message, SIA-201 in
// a block of measured output. A key in prose, a list item or a link destination stays.
//
// Parsed with goldmark (CommonMark) rather than matched with a pattern, because the
// edges are where a pattern goes wrong: a fence inside a blockquote, a tilde fence, a
// span opened by two backticks, a stray backtick that opens nothing, a list
// continuation that only looks indented.
func BlankMarkdownCode(markdown string) string {
	src := []byte(markdown)
	doc := goldmark.DefaultParser().Parse(text.NewReader(src))

	blank := func(seg text.Segment) {
		for i := seg.Start; i < seg.Stop && i < len(src); i++ {
			if src[i] != '\n' && src[i] != '\r' {
				src[i] = ' '
			}
		}
	}

	// The walker never returns an error: the visitor below returns none.
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch n.Kind() {
		case ast.KindFencedCodeBlock, ast.KindCodeBlock:
			lines := n.Lines()
			for i := range lines.Len() {
				blank(lines.At(i))
			}

			return ast.WalkSkipChildren, nil
		case ast.KindCodeSpan:
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					blank(t.Segment)
				}
			}

			return ast.WalkSkipChildren, nil
		}

		return ast.WalkContinue, nil
	})

	return string(src)
}
