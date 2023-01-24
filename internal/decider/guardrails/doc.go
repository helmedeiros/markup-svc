// Package guardrails provides a decorator at the markup.Decider port
// that vetoes Decisions outside a configured envelope. See ADR-0014.
//
// The package exposes:
//
//   - Rule: a single-method port whose Check returns (allowed, reason)
//     for a given (Decision, Request) pair. Implementations carry the
//     deployment-specific veto logic.
//   - Decider: a markup.Decider that wraps an inner Decider with a
//     sequence of Rules. On allowed: the Decision is returned
//     unchanged. On vetoed: the zero Decision is returned with an
//     error wrapping ErrGuardrailViolation so callers can distinguish
//     guardrail vetoes from other engine errors via errors.Is.
//   - ErrGuardrailViolation: the sentinel.
//   - FactorRange, AllowedCountries, RequiredFields: three shipped
//     Rule implementations that cover the common cases.
//
// Default composition per ADR-0014 places guardrails inside the OTel
// decorator so vetoes record as codes.Error on the trace span; the
// metrics decorator (library-provided per ADR-0010) classifies vetoes
// as Err with the wrapped reason preserved on event.Err when an
// operator wires a Sink.
package guardrails
