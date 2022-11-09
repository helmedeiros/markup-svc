package snapshot_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/load"
	"github.com/helmedeiros/markup-svc/internal/markup"
	"github.com/helmedeiros/markup-svc/internal/snapshot"
)

const csvRules = `name,condition,factor,priority
de_enterprise,country == 'DE' AND customer_tier == 'enterprise',1.20,0
br_consumer,country == 'BR' AND customer_tier == 'consumer',1.05,0
fr_any,country == 'FR',1.10,0
`

func loadRules(t *testing.T) []load.Rule {
	t.Helper()
	rules, err := load.FromCSV(strings.NewReader(csvRules))
	if err != nil {
		t.Fatalf("load.FromCSV: %v", err)
	}
	return rules
}

func TestBuildHappyPath(t *testing.T) {
	rules := loadRules(t)
	s, err := snapshot.Build(rules, "v1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if s.FormatVersion != snapshot.FormatVersion {
		t.Errorf("FormatVersion = %d, want %d", s.FormatVersion, snapshot.FormatVersion)
	}
	if s.ModelVersion != "v1" {
		t.Errorf("ModelVersion = %q, want \"v1\"", s.ModelVersion)
	}
	if len(s.Factors) != 3 {
		t.Errorf("len(Factors) = %d, want 3", len(s.Factors))
	}
	for _, r := range rules {
		if s.Factors[r.Name] != r.Factor {
			t.Errorf("Factors[%q] = %v, want %v", r.Name, s.Factors[r.Name], r.Factor)
		}
	}
	if s.EngineSnapshot == nil || len(s.EngineSnapshot.Rules) != 3 {
		t.Errorf("EngineSnapshot.Rules = %+v, want 3", s.EngineSnapshot)
	}
}

func TestBuildPropagatesEngineError(t *testing.T) {
	const bad = `name,condition,factor,priority
neg_only,NOT country == 'XX',1.0,0
`
	rules, err := load.FromCSV(strings.NewReader(bad))
	if err != nil {
		t.Fatalf("FromCSV: %v", err)
	}
	_, err = snapshot.Build(rules, "v1")
	if err == nil {
		t.Fatal("want error for non-indexable rule, got nil")
	}
	if !strings.Contains(err.Error(), "neg_only") {
		t.Errorf("error %q should name the offending rule", err.Error())
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	rules := loadRules(t)
	s, err := snapshot.Build(rules, "v1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var buf bytes.Buffer
	if err := snapshot.Write(&buf, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := snapshot.Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.ModelVersion != s.ModelVersion {
		t.Errorf("ModelVersion lost: got %q, want %q", got.ModelVersion, s.ModelVersion)
	}
	if len(got.Factors) != len(s.Factors) {
		t.Errorf("Factors length lost: got %d, want %d", len(got.Factors), len(s.Factors))
	}
}

func TestReadRejectsFormatVersionMismatch(t *testing.T) {
	body := `{"formatVersion":999,"modelVersion":"v1","factors":{},"engineSnapshot":null}`
	_, err := snapshot.Read(strings.NewReader(body))
	if !errors.Is(err, snapshot.ErrFormatVersionMismatch) {
		t.Fatalf("want ErrFormatVersionMismatch, got %v", err)
	}
}

func TestReadRejectsMalformedJSON(t *testing.T) {
	_, err := snapshot.Read(strings.NewReader("not json"))
	if err == nil {
		t.Fatal("want error for malformed JSON, got nil")
	}
}

// TestLoadIntoIndexedDeciderEquivalentToNewFromRules is the load-bearing
// test for ADR-0007: a Decider built from a snapshot must return the
// same Decision (Rule + MarkupFactor) as one built directly from the
// rules across a representative Request matrix.
func TestLoadIntoIndexedDeciderEquivalentToNewFromRules(t *testing.T) {
	rules := loadRules(t)
	s, err := snapshot.Build(rules, "v1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	fromRules, err := indexed.NewFromRules(rules, "v1")
	if err != nil {
		t.Fatalf("indexed.NewFromRules: %v", err)
	}
	fromSnap, err := snapshot.LoadIntoIndexedDecider(s)
	if err != nil {
		t.Fatalf("LoadIntoIndexedDecider: %v", err)
	}

	cases := []markup.Request{
		{Country: "DE", CustomerTier: "enterprise"},
		{Country: "BR", CustomerTier: "consumer"},
		{Country: "FR", CustomerTier: "enterprise"},
		{Country: "DE", CustomerTier: "consumer"}, // no match
		{Country: "ZZ"},                           // no match
	}
	for i, req := range cases {
		rD, rErr := fromRules.Decide(context.Background(), req)
		sD, sErr := fromSnap.Decide(context.Background(), req)
		if (rErr == nil) != (sErr == nil) {
			t.Errorf("[%d] err mismatch for %+v: rules=%v snapshot=%v", i, req, rErr, sErr)
			continue
		}
		if rErr != nil {
			continue
		}
		if rD.Rule != sD.Rule {
			t.Errorf("[%d] rule divergence for %+v: rules=%q snapshot=%q",
				i, req, rD.Rule, sD.Rule)
		}
		if rD.MarkupFactor != sD.MarkupFactor {
			t.Errorf("[%d] factor divergence for %+v: rules=%v snapshot=%v",
				i, req, rD.MarkupFactor, sD.MarkupFactor)
		}
		if sD.ModelVersion != "v1" {
			t.Errorf("[%d] snapshot ModelVersion = %q, want \"v1\"", i, sD.ModelVersion)
		}
		if sD.EngineAdapter != "*indexed.Engine" {
			t.Errorf("[%d] snapshot EngineAdapter = %q, want \"*indexed.Engine\"", i, sD.EngineAdapter)
		}
	}
}

func TestLoadIntoIndexedDeciderRejectsMissingFactor(t *testing.T) {
	rules := loadRules(t)
	s, err := snapshot.Build(rules, "v1")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	delete(s.Factors, "fr_any")

	_, err = snapshot.LoadIntoIndexedDecider(s)
	if !errors.Is(err, snapshot.ErrMissingFactor) {
		t.Fatalf("want ErrMissingFactor, got %v", err)
	}
	if !strings.Contains(err.Error(), "fr_any") {
		t.Errorf("error %q should name the offending rule", err.Error())
	}
}

func TestLoadIntoIndexedDeciderRejectsNilEngineSnapshot(t *testing.T) {
	s := &snapshot.Snapshot{
		FormatVersion: snapshot.FormatVersion,
		ModelVersion:  "v1",
		Factors:       map[string]float64{},
	}
	_, err := snapshot.LoadIntoIndexedDecider(s)
	if err == nil {
		t.Fatal("want error for nil engine snapshot, got nil")
	}
}
