// Package router decorates a set of markup.Decider instances with a
// routing layer that picks which Decider serves each Decide call via
// a pluggable Policy. The Router stamps Decision.ModelVersion and
// Decision.Experiment from the chosen Route post-Decide so the
// router is the source of truth for routing labels -- inner Deciders
// cannot accidentally erase them. See ADR-0011.
package router

import (
	"context"
	"errors"
	"fmt"

	breengine "github.com/helmedeiros/bre-go/engine"

	"github.com/helmedeiros/markup-svc/internal/markup"
)

// Route bundles a Decider with the (ModelVersion, Variant) labels
// the Router stamps on every Decision served by that Decider.
// Variant is the empty string for non-A/B routes.
type Route struct {
	ModelVersion string
	Variant      string
	Decider      markup.Decider
}

// Policy picks which Route serves a given Request. Implementations
// own the deployment-specific routing logic; the router package
// ships three: HashCorrelationPolicy, HashFieldPolicy, DefaultPolicy.
type Policy interface {
	Choose(ctx context.Context, req markup.Request, routes []Route) (Route, error)
}

// ErrNoRoute is returned when Policy.Choose returns an error or no
// route is selected. Distinct from markup.ErrNoMatch because the
// observability semantics differ -- routing failure is a server-side
// problem (misconfigured router, empty route set), domain miss is
// the engine evaluating every rule and none firing.
var ErrNoRoute = errors.New("router: no route matched the request")

// Router implements markup.Decider. Holds a set of Routes plus a
// Policy that picks which one serves each Decide. Routes and Policy
// are read-only after construction; concurrent Decides need no
// locking. See ADR-0011.
type Router struct {
	routes []Route
	policy Policy
}

// New returns a Router wired to the given routes and policy. Both
// arguments are captured by value; mutating the caller's routes slice
// after New has no effect on the Router. Passing zero routes is
// allowed -- every Decide will return ErrNoRoute -- but a deployment
// passing zero routes is almost certainly a wiring bug.
func New(routes []Route, policy Policy) *Router {
	// Defensive copy so a caller mutating their slice afterwards
	// cannot change which routes the Router dispatches to.
	copied := make([]Route, len(routes))
	copy(copied, routes)
	return &Router{routes: copied, policy: policy}
}

// Decide implements markup.Decider. Calls the policy to pick a Route,
// dispatches to that Route's Decider, then stamps the Route's
// ModelVersion + Variant on the returned Decision -- the router is
// the source of truth for routing labels, so inner Deciders that
// happen to set these fields are silently overridden.
//
// Policy errors map to ErrNoRoute (wrapped for errors.Is). Inner
// Decider errors (including markup.ErrNoMatch) propagate unchanged
// so domain miss vs routing failure stays distinguishable.
func (r *Router) Decide(ctx context.Context, req markup.Request) (markup.Decision, error) {
	chosen, err := r.policy.Choose(ctx, req, r.routes)
	if err != nil {
		return markup.Decision{}, fmt.Errorf("%w: %s", ErrNoRoute, err.Error())
	}
	decision, err := chosen.Decider.Decide(ctx, req)
	if err != nil {
		return decision, err
	}
	decision.ModelVersion = chosen.ModelVersion
	decision.Experiment = chosen.Variant
	return decision, nil
}

// HashCorrelationPolicy is the sticky-by-correlation-ID policy:
// hashes engine.CorrelationIDFromContext(ctx) with FNV-1a, picks the
// route at index hash % len(routes). Same correlation ID -> same
// route across retries. When ctx carries no correlation ID, falls
// back to the first route (the deterministic-default behaviour
// operators expect for unauthenticated probes / health checks).
type HashCorrelationPolicy struct{}

// Choose implements Policy.
func (HashCorrelationPolicy) Choose(ctx context.Context, _ markup.Request, routes []Route) (Route, error) {
	if len(routes) == 0 {
		return Route{}, errors.New("no routes configured")
	}
	cid := breengine.CorrelationIDFromContext(ctx)
	if cid == "" {
		return routes[0], nil
	}
	idx := hashFNV1a(cid) % uint64(len(routes))
	return routes[idx], nil
}

// HashFieldPolicy is the sticky-by-Request-field policy. The Field
// closure returns the string to hash for each Request -- callers
// pick which Request field carries the stickiness axis (ProductID,
// Country, etc.). Empty strings fall through to the first route.
type HashFieldPolicy struct {
	Field func(markup.Request) string
}

// Choose implements Policy.
func (p HashFieldPolicy) Choose(_ context.Context, req markup.Request, routes []Route) (Route, error) {
	if len(routes) == 0 {
		return Route{}, errors.New("no routes configured")
	}
	if p.Field == nil {
		return Route{}, errors.New("HashFieldPolicy.Field is nil")
	}
	key := p.Field(req)
	if key == "" {
		return routes[0], nil
	}
	idx := hashFNV1a(key) % uint64(len(routes))
	return routes[idx], nil
}

// DefaultPolicy always returns the first route. Useful when the
// router is wired with a single route (placeholder for future
// multi-route deployments) or when traffic should not be split.
type DefaultPolicy struct{}

// Choose implements Policy.
func (DefaultPolicy) Choose(_ context.Context, _ markup.Request, routes []Route) (Route, error) {
	if len(routes) == 0 {
		return Route{}, errors.New("no routes configured")
	}
	return routes[0], nil
}

// hashFNV1a is an allocation-free FNV-1a 64-bit hash over a string.
// The standard library's hash/fnv.New64a() allocates a Hash object
// per call; this version operates directly on the input bytes so the
// policy path stays heap-clean.
func hashFNV1a(s string) uint64 {
	const (
		offset uint64 = 14695981039346656037
		prime  uint64 = 1099511628211
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
