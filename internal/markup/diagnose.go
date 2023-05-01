package markup

// IssueKind names a class of rule-set problem. The string values are
// the canonical wire identifier returned by /admin/diagnose so
// operator dashboards can filter on a stable key. See ADR-0025.
type IssueKind string

const (
	IssueEmptyRuleSet      IssueKind = "empty_rule_set"
	IssueDuplicateName     IssueKind = "duplicate_name"
	IssueDuplicatePriority IssueKind = "duplicate_priority"
	IssueInvalidFactor     IssueKind = "invalid_factor"
	IssueNoOpFactor        IssueKind = "no_op_factor"
	IssueEmptyCondition    IssueKind = "empty_condition"
)

type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	}
	return "unknown"
}

// Issue is the unit of diagnosis output. Rule is the name of the
// offending rule when applicable; empty for set-level issues.
type Issue struct {
	Kind     IssueKind `json:"kind"`
	Severity Severity  `json:"-"`
	Rule     string    `json:"rule,omitempty"`
	Detail   string    `json:"detail"`
}

// Diagnosis is the collection of issues from one diagnose pass over
// a rule set. IsHealthy is true when no SeverityError issues are
// present; SeverityWarning issues are informational and never make
// IsHealthy false.
type Diagnosis struct {
	Issues []Issue
}

func (d Diagnosis) Errors() []Issue {
	out := make([]Issue, 0, len(d.Issues))
	for _, i := range d.Issues {
		if i.Severity == SeverityError {
			out = append(out, i)
		}
	}
	return out
}

func (d Diagnosis) Warnings() []Issue {
	out := make([]Issue, 0, len(d.Issues))
	for _, i := range d.Issues {
		if i.Severity == SeverityWarning {
			out = append(out, i)
		}
	}
	return out
}

func (d Diagnosis) IsHealthy() bool {
	return len(d.Errors()) == 0
}
