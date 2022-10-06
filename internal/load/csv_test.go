package load_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/helmedeiros/bre-go/engine/parser"

	"github.com/helmedeiros/markup-svc/internal/load"
)

const happyCSV = `name,condition,factor,priority
enterprise,customer_tier == 'enterprise',1.15,10
br_peak,country == 'BR' AND time_window == 'peak',1.08,5
`

func TestFromCSVHappyPath(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(happyCSV))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("len(rules) = %d, want 2", len(rules))
	}
	if rules[0].Name != "enterprise" || rules[0].Factor != 1.15 || rules[0].Priority != 10 {
		t.Errorf("rules[0] = %+v", rules[0])
	}
	if rules[1].Name != "br_peak" || rules[1].Factor != 1.08 || rules[1].Priority != 5 {
		t.Errorf("rules[1] = %+v", rules[1])
	}
	if _, ok := rules[0].Condition.(parser.StringCondition); !ok {
		t.Errorf("rules[0].Condition = %T, want StringCondition", rules[0].Condition)
	}
	if _, ok := rules[1].Condition.(parser.AndCondition); !ok {
		t.Errorf("rules[1].Condition = %T, want AndCondition", rules[1].Condition)
	}
}

func TestFromCSVOnlyHeaderReturnsEmpty(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader("name,condition,factor,priority\n"))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("len(rules) = %d, want 0", len(rules))
	}
}

func TestFromCSVEmptyInputReturnsEmpty(t *testing.T) {
	rules, err := load.FromCSV(strings.NewReader(""))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("len(rules) = %d, want 0", len(rules))
	}
}

func TestFromCSVMissingColumnFailsWithRow(t *testing.T) {
	input := "name,condition,factor,priority\nname,too,few\n"
	_, err := load.FromCSV(strings.NewReader(input))
	var le *load.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if le.Row != 2 {
		t.Errorf("Row = %d, want 2", le.Row)
	}
	if !strings.Contains(le.Error(), "row 2") {
		t.Errorf("Error() = %q, want to contain \"row 2\"", le.Error())
	}
}

func TestFromCSVBadConditionFailsWithRow(t *testing.T) {
	input := "name,condition,factor,priority\nx,not a valid expression,1.0,0\n"
	_, err := load.FromCSV(strings.NewReader(input))
	var le *load.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if le.Row != 2 {
		t.Errorf("Row = %d, want 2", le.Row)
	}
	var pe *parser.ParseError
	if !errors.As(err, &pe) {
		t.Errorf("err = %v, want underlying *parser.ParseError", err)
	}
}

func TestFromCSVBadFactorFailsWithRow(t *testing.T) {
	input := `name,condition,factor,priority
x,country == 'BR',not-a-number,0
`
	_, err := load.FromCSV(strings.NewReader(input))
	var le *load.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if le.Row != 2 {
		t.Errorf("Row = %d, want 2", le.Row)
	}
	if !strings.Contains(le.Error(), "factor") {
		t.Errorf("Error() = %q, want to contain \"factor\"", le.Error())
	}
}

func TestFromCSVBadPriorityFailsWithRow(t *testing.T) {
	input := `name,condition,factor,priority
x,country == 'BR',1.0,not-an-int
`
	_, err := load.FromCSV(strings.NewReader(input))
	var le *load.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if le.Row != 2 {
		t.Errorf("Row = %d, want 2", le.Row)
	}
	if !strings.Contains(le.Error(), "priority") {
		t.Errorf("Error() = %q, want to contain \"priority\"", le.Error())
	}
}

func TestFromCSVMalformedCSVFailsWithRow(t *testing.T) {
	input := "name,condition,factor,priority\n\"unterminated,1.0,0\n"
	_, err := load.FromCSV(strings.NewReader(input))
	var le *load.LoadError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LoadError", err)
	}
	if le.Row == 0 {
		t.Errorf("Row = 0, want >= 2 (malformed CSV row)")
	}
}

func TestLoadErrorMessageWithoutRow(t *testing.T) {
	le := &load.LoadError{Row: 0, Err: errors.New("file open failed")}
	got := le.Error()
	if got != "load: file open failed" {
		t.Errorf("Error() = %q, want %q", got, "load: file open failed")
	}
}

func TestLoadErrorMessageWithRow(t *testing.T) {
	le := &load.LoadError{Row: 7, Err: errors.New("oops")}
	got := le.Error()
	if got != "load: row 7: oops" {
		t.Errorf("Error() = %q, want %q", got, "load: row 7: oops")
	}
}

func TestLoadErrorUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	le := &load.LoadError{Row: 1, Err: sentinel}
	if !errors.Is(le, sentinel) {
		t.Fatal("errors.Is must reach the wrapped error")
	}
}

func BenchmarkFromCSV(b *testing.B) {
	const tmpl = `name,condition,factor,priority
`
	var sb strings.Builder
	sb.WriteString(tmpl)
	for i := 0; i < 50; i++ {
		sb.WriteString(`r,customer_tier == 'enterprise' AND country IN ('BR', 'DE', 'FR'),1.10,0
`)
	}
	data := sb.String()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = load.FromCSV(strings.NewReader(data))
	}
}
