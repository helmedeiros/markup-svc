package markup

import "fmt"

// InvalidRuleSetError marks a rule-set load failure that is the
// caller's fault (the supplied CSV / snapshot is malformed,
// unparseable, or fails validation). Wraps the underlying load /
// parser error via Unwrap. The httpapi handlers detect this via
// errors.As to return 400 instead of 500 — a malformed payload is
// not a server fault. See ADR-0027.
type InvalidRuleSetError struct {
	Path string
	Err  error
}

func (e *InvalidRuleSetError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("invalid rule set: %v", e.Err)
	}
	return fmt.Sprintf("invalid rule set %q: %v", e.Path, e.Err)
}

func (e *InvalidRuleSetError) Unwrap() error { return e.Err }
