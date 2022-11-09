// Package snapshot persists a built indexed rule set to disk and
// reconstructs it at cold start. The on-disk format is JSON wrapping
// bre-go's engine/indexed.Snapshot plus a Factors map that carries
// per-rule markup factors so the rebuild callback can re-attach the
// Action closure for each rule by name. See ADR-0007.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	breindexed "github.com/helmedeiros/bre-go/engine/indexed"

	mkindexed "github.com/helmedeiros/markup-svc/internal/decider/indexed"
	"github.com/helmedeiros/markup-svc/internal/load"
)

// FormatVersion is the markup-side snapshot schema version. Read
// refuses any other value so an older binary refuses to load a
// future-format snapshot. The embedded bre-go Snapshot has its own
// FormatVersion checked by bre-go's LoadSnapshot.
const FormatVersion = 1

// Snapshot is the markup-side on-disk wrapper around bre-go's typed
// indexed snapshot. Factors carries the per-rule markup factor keyed
// by rule name so LoadIntoIndexedDecider can rebuild the Action
// closures that bre-go cannot serialize.
type Snapshot struct {
	FormatVersion  int                `json:"formatVersion"`
	ModelVersion   string             `json:"modelVersion"`
	Factors        map[string]float64 `json:"factors"`
	EngineSnapshot *breindexed.Snapshot `json:"engineSnapshot"`
}

// ErrFormatVersionMismatch is returned by Read when the snapshot's
// FormatVersion does not match this binary's FormatVersion.
var ErrFormatVersionMismatch = errors.New("snapshot: format version mismatch")

// ErrMissingFactor is returned by LoadIntoIndexedDecider when the
// engine snapshot lists a rule whose name has no entry in Factors.
// A silent zero-factor Decision would be a worse failure mode.
var ErrMissingFactor = errors.New("snapshot: rule has no factor")

// Build constructs a Snapshot from loader-side rules and the model
// version tag. Internally it builds an indexed.Engine, exports its
// snapshot, and pairs it with the factor map keyed by rule name.
func Build(rules []load.Rule, modelVersion string) (*Snapshot, error) {
	e := breindexed.New()
	factors := make(map[string]float64, len(rules))
	for _, r := range rules {
		factor := r.Factor
		if err := e.AddRule(breindexed.Rule{
			Name:  r.Name,
			Match: r.Condition,
			Action: func(in interface{}) interface{} {
				return factor
			},
		}); err != nil {
			return nil, fmt.Errorf("snapshot: add rule %q: %w", r.Name, err)
		}
		factors[r.Name] = r.Factor
	}
	engineSnap, err := e.ExportSnapshot()
	if err != nil {
		return nil, fmt.Errorf("snapshot: export: %w", err)
	}
	return &Snapshot{
		FormatVersion:  FormatVersion,
		ModelVersion:   modelVersion,
		Factors:        factors,
		EngineSnapshot: engineSnap,
	}, nil
}

// Write serializes s as indented JSON to w.
func Write(w io.Writer, s *Snapshot) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("snapshot: write: %w", err)
	}
	return nil
}

// Read deserializes a Snapshot from r and validates FormatVersion.
func Read(r io.Reader) (*Snapshot, error) {
	var s Snapshot
	if err := json.NewDecoder(r).Decode(&s); err != nil {
		return nil, fmt.Errorf("snapshot: read: %w", err)
	}
	if s.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrFormatVersionMismatch, s.FormatVersion, FormatVersion)
	}
	return &s, nil
}

// LoadIntoIndexedDecider rebuilds an indexed.Decider from s. The
// rebuild callback map closes over each rule's factor from s.Factors;
// rules listed in the engine snapshot but missing from Factors
// surface as ErrMissingFactor rather than silent zero-factor Decisions.
func LoadIntoIndexedDecider(s *Snapshot) (*mkindexed.Decider, error) {
	if s.EngineSnapshot == nil {
		return nil, fmt.Errorf("snapshot: nil engine snapshot")
	}
	rebuild := make(map[string]breindexed.RuleCallbacks, len(s.EngineSnapshot.Rules))
	for _, r := range s.EngineSnapshot.Rules {
		factor, ok := s.Factors[r.Name]
		if !ok {
			return nil, fmt.Errorf("%w: %q", ErrMissingFactor, r.Name)
		}
		rebuild[r.Name] = breindexed.RuleCallbacks{
			Action: func(in interface{}) interface{} {
				return factor
			},
		}
	}
	engine, err := breindexed.LoadSnapshot(s.EngineSnapshot, rebuild)
	if err != nil {
		return nil, fmt.Errorf("snapshot: load: %w", err)
	}
	return mkindexed.NewFromEngine(engine, s.ModelVersion), nil
}
