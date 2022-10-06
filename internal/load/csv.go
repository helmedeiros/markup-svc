// Package load reads markup rule sets from disk. The on-disk format
// (CSV) is documented in ADR-0002; the loader produces an in-memory
// []Rule with the condition column pre-compiled into a typed
// parser.Condition tree.
package load

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"

	"github.com/helmedeiros/bre-go/engine/parser"
)

// Rule is the loader-side rule record. Each row in the source CSV
// produces one Rule with the condition column already compiled.
// Adapter constructors wrap Condition via parser.AsRuleCondition
// with the project's markup.FactOf converter to satisfy whichever
// rule shape their bre-go engine expects.
type Rule struct {
	Name      string
	Condition parser.Condition
	Factor    float64
	Priority  int
}

// LoadError reports a per-row failure with the 1-indexed row number
// from the source. Row is 0 for file-level failures.
type LoadError struct {
	Row int
	Err error
}

// Error implements the error interface.
func (e *LoadError) Error() string {
	if e.Row == 0 {
		return fmt.Sprintf("load: %v", e.Err)
	}
	return fmt.Sprintf("load: row %d: %v", e.Row, e.Err)
}

// Unwrap supports errors.Is / errors.As against the underlying error.
func (e *LoadError) Unwrap() error { return e.Err }

// FromCSV reads r as a CSV rule file with a single header row and the
// columns name, condition, factor, priority. The header is skipped.
// Subsequent rows produce one Rule each with Condition compiled by
// parser.ParseToCondition. Stops at the first malformed row.
func FromCSV(r io.Reader) ([]Rule, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1

	var out []Rule
	rowNum := 0
	for {
		rowNum++
		cols, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, &LoadError{Row: rowNum, Err: err}
		}
		if rowNum == 1 {
			continue
		}
		if len(cols) < 4 {
			return nil, &LoadError{Row: rowNum, Err: fmt.Errorf("expected 4 columns, got %d", len(cols))}
		}
		cond, err := parser.ParseToCondition(cols[1])
		if err != nil {
			return nil, &LoadError{Row: rowNum, Err: fmt.Errorf("condition: %w", err)}
		}
		factor, err := strconv.ParseFloat(cols[2], 64)
		if err != nil {
			return nil, &LoadError{Row: rowNum, Err: fmt.Errorf("factor: %w", err)}
		}
		prio, err := strconv.Atoi(cols[3])
		if err != nil {
			return nil, &LoadError{Row: rowNum, Err: fmt.Errorf("priority: %w", err)}
		}
		out = append(out, Rule{
			Name:      cols[0],
			Condition: cond,
			Factor:    factor,
			Priority:  prio,
		})
	}
}
