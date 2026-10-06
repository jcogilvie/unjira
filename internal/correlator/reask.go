package correlator

import (
	"fmt"
	"strings"
)

// The helpers every re-ask site shares: matching (classifyCandidates), the omission
// re-ask (recoverOmittedEvents) and the dispute re-ask (resolveDisputes). Each site
// owns its own loop and budget; these only keep the wording of a refusal, in a
// prompt and in an error, the same everywhere.

// writeRefusal appends a refused response and the parser's reason to a re-ask
// prompt. Only the LATEST refusal is ever quoted: the model is answering what it
// just said, and an older refusal's reason may no longer apply.
func writeRefusal(b *strings.Builder, refused string, reason error) {
	b.WriteString("\n\n## Your previous response could not be used\n\n")
	fmt.Fprintf(b, "It was refused because: %v\n\n", reason)
	b.WriteString("The refused response:\n\n")
	b.WriteString(refused)
}

// refusalsError is the error for a re-ask budget spent on responses the parser
// refused: every refusal's reason, in call order, so the error shows whether the
// model kept making one mistake or a different one each time. Unwrap exposes them
// all to errors.Is and errors.As.
type refusalsError struct {
	// what names the problem being re-asked ("match response for narrative 7").
	what string
	// key is the config key that sets the budget, so the error says what to raise.
	key     string
	budget  int
	reasons []error
}

func (e *refusalsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s refused %d time(s), exhausting the re-ask budget of %d (%s)",
		e.what, len(e.reasons), e.budget, e.key)
	for i, r := range e.reasons {
		fmt.Fprintf(&b, "; response %d: %v", i+1, r)
	}

	return b.String()
}

func (e *refusalsError) Unwrap() []error { return e.reasons }
