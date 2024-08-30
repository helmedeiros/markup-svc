package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/decider/shadow"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type fakeShadowMetrics struct {
	mu                 sync.Mutex
	agreeTrue, agreeFalse, timeouts, errors int
	oneSidedChampion, oneSidedChallenger    int
	sampledTrue, sampledFalse               int
	deltas                                  []float64
}

func (f *fakeShadowMetrics) RecordAgreement(agree bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if agree {
		f.agreeTrue++
	} else {
		f.agreeFalse++
	}
}
func (f *fakeShadowMetrics) RecordOneSided(side string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch side {
	case "champion_only":
		f.oneSidedChampion++
	case "challenger_only":
		f.oneSidedChallenger++
	}
}
func (f *fakeShadowMetrics) RecordTimeout()                  { f.mu.Lock(); f.timeouts++; f.mu.Unlock() }
func (f *fakeShadowMetrics) RecordError()                    { f.mu.Lock(); f.errors++; f.mu.Unlock() }
func (f *fakeShadowMetrics) RecordFactorDelta(d float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deltas = append(f.deltas, d)
}
func (f *fakeShadowMetrics) RecordSampled(sampled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sampled {
		f.sampledTrue++
	} else {
		f.sampledFalse++
	}
}

type metricsSnapshot struct {
	agreeTrue, agreeFalse, timeouts, errors int
	oneSidedChampion, oneSidedChallenger    int
	sampledTrue, sampledFalse               int
	deltas                                  []float64
}

func (f *fakeShadowMetrics) snapshot() metricsSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return metricsSnapshot{
		agreeTrue:           f.agreeTrue,
		agreeFalse:          f.agreeFalse,
		timeouts:            f.timeouts,
		errors:              f.errors,
		oneSidedChampion:    f.oneSidedChampion,
		oneSidedChallenger:  f.oneSidedChallenger,
		sampledTrue:         f.sampledTrue,
		sampledFalse:        f.sampledFalse,
		deltas:              append([]float64(nil), f.deltas...),
	}
}

type fixedDecider struct {
	factor float64
	rule   string
	err    error
}

func (f fixedDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	if f.err != nil {
		return markup.Decision{}, f.err
	}
	return markup.Decision{MarkupFactor: f.factor, Rule: f.rule}, nil
}

func decideBody() []byte {
	return []byte(`{"product_id":"p","amount":100}`)
}

func runDecide(t *testing.T, champion markup.Decider, holder httpapi.ChallengerHolder, m *fakeShadowMetrics) {
	t.Helper()
	h := httpapi.Decide(champion, httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 1.0))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never observed")
}

func TestDecide_NoShadowWiredIsBitForBitOldBehaviour(t *testing.T) {
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"})
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDecide_ShadowFastPathWhenHolderEmpty(t *testing.T) {
	holder := shadow.New()
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{factor: 1.2, rule: "alpha"}, holder, m)
	// Wait a tiny bit; nothing should fire because holder is empty.
	time.Sleep(20 * time.Millisecond)
	snap := m.snapshot()
	if snap.agreeTrue+snap.agreeFalse+snap.timeouts+snap.errors+snap.oneSidedChampion+snap.oneSidedChallenger > 0 {
		t.Fatalf("fast path leaked metrics: %+v", snap)
	}
}

func TestDecide_AgreementWhenFactorsMatch(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "alpha"})
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{factor: 1.2, rule: "alpha"}, holder, m)
	waitFor(t, func() bool { return m.snapshot().agreeTrue == 1 })
}

func TestDecide_DisagreementRecordsFactorDelta(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.5, rule: "alpha"})
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{factor: 1.2, rule: "alpha"}, holder, m)
	waitFor(t, func() bool { return m.snapshot().agreeFalse == 1 && len(m.snapshot().deltas) == 1 })
	delta := m.snapshot().deltas[0]
	if delta < 0.29 || delta > 0.31 {
		t.Fatalf("delta=%v want ~0.3", delta)
	}
}

func TestDecide_OneSidedWhenChampionFiresAndChallengerDeclines(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{err: markup.ErrNoMatch})
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{factor: 1.2, rule: "alpha"}, holder, m)
	waitFor(t, func() bool { return m.snapshot().oneSidedChampion == 1 })
}

func TestDecide_OneSidedWhenChallengerFiresAndChampionDeclines(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "challenger"})
	m := &fakeShadowMetrics{}
	champion := fixedDecider{err: markup.ErrNoMatch}
	runDecide(t, champion, holder, m)
	waitFor(t, func() bool { return m.snapshot().oneSidedChallenger == 1 })
}

func TestDecide_BothDeclineCountsAsAgreement(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{err: markup.ErrNoMatch})
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{err: markup.ErrNoMatch}, holder, m)
	waitFor(t, func() bool { return m.snapshot().agreeTrue == 1 })
}

type slowDecider struct{ sleep time.Duration }

func (s slowDecider) Decide(ctx context.Context, _ markup.Request) (markup.Decision, error) {
	select {
	case <-time.After(s.sleep):
		return markup.Decision{MarkupFactor: 1.0, Rule: "slow"}, nil
	case <-ctx.Done():
		return markup.Decision{}, ctx.Err()
	}
}

func TestDecide_ChallengerTimeoutCountsAsTimeout(t *testing.T) {
	holder := shadow.New()
	holder.Load(slowDecider{sleep: 200 * time.Millisecond})
	m := &fakeShadowMetrics{}
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"}, httpapi.WithShadow(holder, m, 5*time.Millisecond, nil, 1.0))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return m.snapshot().timeouts == 1 })
}

type erroringDecider struct{}

func (erroringDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{}, errors.New("challenger boom")
}

type explodingDecider struct{}

func (explodingDecider) Decide(_ context.Context, _ markup.Request) (markup.Decision, error) {
	return markup.Decision{}, errors.New("champion boom")
}

func TestDecide_ChampionErrorSkipsShadow(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "challenger"})
	m := &fakeShadowMetrics{}
	runDecide(t, explodingDecider{}, holder, m)
	time.Sleep(30 * time.Millisecond)
	snap := m.snapshot()
	if snap.agreeTrue+snap.agreeFalse+snap.oneSidedChampion+snap.oneSidedChallenger+snap.timeouts+snap.errors > 0 {
		t.Fatalf("shadow ran on champion error: %+v", snap)
	}
}

func TestDecide_ChallengerErrorCountsAsError(t *testing.T) {
	holder := shadow.New()
	holder.Load(erroringDecider{})
	m := &fakeShadowMetrics{}
	runDecide(t, fixedDecider{factor: 1.2, rule: "alpha"}, holder, m)
	waitFor(t, func() bool { return m.snapshot().errors == 1 })
}

func TestDecide_SampleRateZeroDisablesComparisonButRecordsSampledFalse(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "alpha"})
	m := &fakeShadowMetrics{}
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 0.0))
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	time.Sleep(30 * time.Millisecond)
	snap := m.snapshot()
	if snap.agreeTrue+snap.agreeFalse > 0 {
		t.Fatalf("sample=0.0 should skip comparison; got agreement: %+v", snap)
	}
	if snap.sampledFalse != 5 {
		t.Fatalf("sampledFalse=%d want 5", snap.sampledFalse)
	}
	if snap.sampledTrue != 0 {
		t.Fatalf("sampledTrue=%d want 0", snap.sampledTrue)
	}
}

func TestDecide_SampleRateOneRunsEveryRequest(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "alpha"})
	m := &fakeShadowMetrics{}
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 1.0))
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	waitFor(t, func() bool { return m.snapshot().sampledTrue == 3 && m.snapshot().agreeTrue == 3 })
}

func TestDecide_SampleRatePartialUsesDeterministicSampler(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "alpha"})
	m := &fakeShadowMetrics{}
	// Every other call returns 0.05 (< 0.5 → sample yes), 0.99 (> 0.5 → sample no)
	calls := 0
	sampler := func() float64 {
		calls++
		if calls%2 == 1 {
			return 0.05
		}
		return 0.99
	}
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 0.5),
		httpapi.WithShadowSampler(sampler))
	for i := 0; i < 4; i++ {
		req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
	waitFor(t, func() bool {
		snap := m.snapshot()
		return snap.sampledTrue == 2 && snap.sampledFalse == 2 && snap.agreeTrue == 2
	})
}
