package markup

import "testing"

func TestDiagnosis_IsHealthy(t *testing.T) {
	cases := []struct {
		name string
		d    Diagnosis
		want bool
	}{
		{"empty", Diagnosis{}, true},
		{"warning only", Diagnosis{Issues: []Issue{{Severity: SeverityWarning}}}, true},
		{"single error", Diagnosis{Issues: []Issue{{Severity: SeverityError}}}, false},
		{"mixed", Diagnosis{Issues: []Issue{{Severity: SeverityWarning}, {Severity: SeverityError}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.d.IsHealthy(); got != c.want {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestDiagnosis_ErrorsWarningsPartition(t *testing.T) {
	d := Diagnosis{Issues: []Issue{
		{Kind: IssueEmptyRuleSet, Severity: SeverityError},
		{Kind: IssueNoOpFactor, Severity: SeverityWarning, Rule: "noop"},
		{Kind: IssueDuplicateName, Severity: SeverityError, Rule: "dup"},
	}}
	if got := len(d.Errors()); got != 2 {
		t.Errorf("Errors len = %d, want 2", got)
	}
	if got := len(d.Warnings()); got != 1 {
		t.Errorf("Warnings len = %d, want 1", got)
	}
}
