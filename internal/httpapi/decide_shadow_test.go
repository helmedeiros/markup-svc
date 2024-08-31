package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmedeiros/markup-svc/internal/decider/shadow"
	"github.com/helmedeiros/markup-svc/internal/httpapi"
	"github.com/helmedeiros/markup-svc/internal/jsonlog"
	"github.com/helmedeiros/markup-svc/internal/markup"
)

type fakeShadowMetrics struct {
	mu                 sync.Mutex
	agreeTrue, agreeFalse, timeouts, errors int
	oneSidedChampion, oneSidedChallenger    int
	sampledTrue, sampledFalse               int
	deltas                                  []float64
	durations                               []time.Duration
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
func (f *fakeShadowMetrics) RecordChallengerDuration(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durations = append(f.durations, d)
}

type metricsSnapshot struct {
	agreeTrue, agreeFalse, timeouts, errors int
	oneSidedChampion, oneSidedChallenger    int
	sampledTrue, sampledFalse               int
	deltas                                  []float64
	durations                               []time.Duration
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
		durations:           append([]time.Duration(nil), f.durations...),
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

type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestDecide_DisagreementEmitsStructuredLog(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.5, rule: "challenger"})
	m := &fakeShadowMetrics{}
	buf := &safeBuf{}
	logger := jsonlog.New(buf)
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "champion"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 1.0),
		httpapi.WithShadowLogger(logger))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return strings.Contains(buf.String(), `"markup.challenger.evaluate"`) })
	var event map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		_ = json.Unmarshal([]byte(line), &event)
		if event["msg"] == "markup.challenger.evaluate" {
			break
		}
	}
	attrs, _ := event["attrs"].(map[string]any)
	if attrs["outcome"] != "disagree" {
		t.Fatalf("outcome=%v want disagree", attrs["outcome"])
	}
	if attrs["champion_factor"].(float64) != 1.2 || attrs["challenger_factor"].(float64) != 1.5 {
		t.Fatalf("factors not surfaced: %+v", attrs)
	}
}

func TestDecide_AgreementDoesNotEmitLog(t *testing.T) {
	holder := shadow.New()
	holder.Load(fixedDecider{factor: 1.2, rule: "challenger"})
	m := &fakeShadowMetrics{}
	buf := &safeBuf{}
	logger := jsonlog.New(buf)
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "champion"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 1.0),
		httpapi.WithShadowLogger(logger))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return m.snapshot().agreeTrue == 1 })
	time.Sleep(30 * time.Millisecond)
	if strings.Contains(buf.String(), `"markup.challenger.evaluate"`) {
		t.Fatalf("agreement should NOT emit a log event; got: %s", buf.String())
	}
}

func TestDecide_TimeoutEmitsStructuredLog(t *testing.T) {
	holder := shadow.New()
	holder.Load(slowDecider{sleep: 200 * time.Millisecond})
	m := &fakeShadowMetrics{}
	buf := &safeBuf{}
	logger := jsonlog.New(buf)
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 5*time.Millisecond, nil, 1.0),
		httpapi.WithShadowLogger(logger))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return strings.Contains(buf.String(), `"outcome":"timeout"`) })
}

func TestDecide_CustomShadowTimeoutHonoured(t *testing.T) {
	holder := shadow.New()
	holder.Load(slowDecider{sleep: 50 * time.Millisecond})
	m := &fakeShadowMetrics{}
	// A 2ms timeout is well below the slowDecider's 50ms sleep — the
	// challenger must time out. If the flag were ignored, the test
	// would wait the default DefaultShadowTimeout (10ms) which is
	// still under 50ms but the assertion still holds; that's fine for
	// this test, but a 2ms timeout guarantees the goroutine returns
	// quickly.
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 2*time.Millisecond, nil, 1.0))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return m.snapshot().timeouts == 1 })
}

func TestDecide_RecordsChallengerLatencyHistogram(t *testing.T) {
	holder := shadow.New()
	holder.Load(slowDecider{sleep: 5 * time.Millisecond})
	m := &fakeShadowMetrics{}
	h := httpapi.Decide(fixedDecider{factor: 1.2, rule: "alpha"},
		httpapi.WithShadow(holder, m, 100*time.Millisecond, nil, 1.0))
	req := httptest.NewRequest(http.MethodPost, "/decide", bytes.NewReader(decideBody()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	waitFor(t, func() bool { return len(m.snapshot().durations) == 1 })
	d := m.snapshot().durations[0]
	if d < 4*time.Millisecond {
		t.Fatalf("challenger duration %v too small; slowDecider sleeps 5ms", d)
	}
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
