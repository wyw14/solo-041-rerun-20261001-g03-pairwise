package power

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

var testDevices = []DeviceProfile{
	{DeviceID: "blower-b1", LineID: "line-1", Kind: model.DeviceBlower, NameplateKW: 75, RatedOutput: 60, MinOutput: 28},
	{DeviceID: "pump-p1", LineID: "line-1", Kind: model.DevicePump, NameplateKW: 22, RatedOutput: 300, MinOutput: 60},
}

func testConfig() Config {
	return Config{
		LimitKW: 100, ReserveKW: 10,
		StaleAfter: 30 * time.Second, StaggerWindow: time.Minute, MaxDefer: 10 * time.Minute,
		WrapTolerance: 0.5,
	}
}

func newTestGovernor(t *testing.T) *Governor {
	t.Helper()
	governor, err := NewGovernor(testDevices, LineBudget{LimitKW: 100, ReserveKW: 10}, testConfig())
	if err != nil {
		t.Fatalf("new governor: %v", err)
	}
	return governor
}

func reading(deviceID string, sequence uint64, energyKWh, powerKW float64, at time.Time) Reading {
	return Reading{DeviceID: deviceID, Sequence: sequence, EnergyKWh: energyKWh, PowerKW: powerKW, ObservedAt: at}
}

func TestHeadroomWithoutTelemetryIsConservative(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	view, err := governor.Headroom("line-1", now)
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}
	if !view.Degraded {
		t.Fatal("missing telemetry must mark the view degraded")
	}
	if view.DrawKW != 97 {
		t.Fatalf("missing telemetry must account nameplate draw, got %v", view.DrawKW)
	}
	if view.AvailableKW != 100-10-97 {
		t.Fatalf("available headroom is wrong: %v", view.AvailableKW)
	}
}

func TestHeadroomUsesFreshMeasurements(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, sample := range []Reading{
		reading("blower-b1", 1, 100, 40, now),
		reading("pump-p1", 1, 50, 10, now),
	} {
		if _, err := governor.Ingest(sample, now); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	view, err := governor.Headroom("line-1", now)
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}
	if view.Degraded || view.DrawKW != 50 || view.AvailableKW != 40 {
		t.Fatalf("unexpected headroom view: %+v", view)
	}
}

func TestStaleReadingFallsBackToNameplate(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 5, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 50, 10, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	later := now.Add(31 * time.Second)
	// Pump reading stays fresh; the blower reading goes stale.
	if _, err := governor.Ingest(reading("pump-p1", 2, 60, 10, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	view, err := governor.Headroom("line-1", later)
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}
	if !view.Degraded {
		t.Fatal("stale telemetry must degrade the view")
	}
	// blower stale → nameplate 75, pump fresh → 10
	if view.DrawKW != 85 {
		t.Fatalf("stale meter must be accounted at nameplate, got %v", view.DrawKW)
	}
}

func TestWrappedMeterIsNotTrusted(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.Ingest(reading("blower-b1", 1, 500, 40, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 50, 10, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	// Register rolls over: energy drops far below the last value.
	meter, err := governor.Ingest(reading("blower-b1", 2, 3, 1, now.Add(time.Second)), now.Add(time.Second))
	if err != nil {
		t.Fatalf("ingest wrap: %v", err)
	}
	if !meter.Wrapped {
		t.Fatal("register rollover must mark the meter wrapped")
	}
	view, err := governor.Headroom("line-1", now.Add(time.Second))
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}
	if view.DrawKW != 75+10 {
		t.Fatalf("wrapped meter must be accounted at nameplate, got %v", view.DrawKW)
	}
	// A consistent follow-up reading re-establishes trust.
	if _, err := governor.Ingest(reading("blower-b1", 3, 4, 8, now.Add(2*time.Second)), now.Add(2*time.Second)); err != nil {
		t.Fatalf("ingest recovery: %v", err)
	}
	view, err = governor.Headroom("line-1", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}
	if view.DrawKW != 8+10 {
		t.Fatalf("recovered meter must be measured again, got %v", view.DrawKW)
	}
}

func TestSequenceRegressionIsIgnored(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.Ingest(reading("blower-b1", 7, 100, 40, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	meter, err := governor.Ingest(reading("blower-b1", 5, 90, 1, now), now)
	if err != nil {
		t.Fatalf("regressed ingest must not fail: %v", err)
	}
	if meter.Last.Sequence != 7 || meter.Last.PowerKW != 40 {
		t.Fatalf("regressed sequence must be ignored: %+v", meter.Last)
	}
}

func TestBlowerClampRespectsBudgetAndFloor(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// blower 40 kW, pump 30 kW → available 20, blower affordable 60 kW → 48 Hz
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 40, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 50, 30, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err := governor.GateBlowerSet("line-1", "blower-b1", 60, 3, false, now)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if decision.Suppression == nil {
		t.Fatal("over-budget setpoint must be clamped")
	}
	if decision.Output != 48 {
		t.Fatalf("clamped output should be 48 Hz, got %v", decision.Output)
	}
	if decision.Suppression.Kind != SuppressionClamped || decision.Suppression.Recovery == "" {
		t.Fatalf("suppression must explain the recovery condition: %+v", decision.Suppression)
	}
	// Crush the budget: floor still wins.
	if _, err := governor.Ingest(reading("pump-p1", 2, 60, 200, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err = governor.GateBlowerSet("line-1", "blower-b1", 60, 3, false, now)
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if decision.Output != 28 {
		t.Fatalf("minimum aeration floor must be protected, got %v", decision.Output)
	}
}

func TestStopAndEmergencyBypassGating(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// No telemetry at all: budget view is fully conservative, yet stops pass.
	decision, err := governor.GateBlowerSet("line-1", "blower-b1", 0, 3, false, now)
	if err != nil {
		t.Fatalf("gate stop: %v", err)
	}
	if decision.Suppression != nil || decision.Output != 0 {
		t.Fatalf("stop commands must never be held back: %+v", decision)
	}
	decision, err = governor.GateBlowerSet("line-1", "blower-b1", 60, 3, true, now)
	if err != nil {
		t.Fatalf("gate emergency: %v", err)
	}
	if decision.Suppression != nil || decision.Output != 60 {
		t.Fatalf("emergency lines must bypass gating: %+v", decision)
	}
}

func TestPumpStartDefersAndReleases(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Blower draws 86 kW, pump meter missing (nameplate 22) → available -18.
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 86, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err := governor.GatePumpStart("line-1", "pump-p1", 3, map[string]string{"number": "4"}, false, now)
	if err != nil {
		t.Fatalf("gate start: %v", err)
	}
	if decision.Suppression == nil || decision.Suppression.Kind != SuppressionDeferred {
		t.Fatalf("pump start must be deferred: %+v", decision)
	}
	deferredID := decision.Suppression.ID
	// Too early: stagger window has not elapsed.
	released, err := governor.ReleaseDue(context.Background(), now.Add(30*time.Second), func(context.Context, Suppression) error {
		t.Fatal("release must not fire before the stagger window")
		return nil
	})
	if err != nil || released != 0 {
		t.Fatalf("early release: %d %v", released, err)
	}
	// Stagger elapsed but headroom still missing: nothing fires.
	released, err = governor.ReleaseDue(context.Background(), now.Add(61*time.Second), func(context.Context, Suppression) error {
		t.Fatal("release must not fire without headroom")
		return nil
	})
	if err != nil || released != 0 {
		t.Fatalf("release without headroom: %d %v", released, err)
	}
	// Load drops: headroom recovers, the deferred start fires.
	later := now.Add(61 * time.Second)
	if _, err := governor.Ingest(reading("blower-b1", 2, 200, 40, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 10, 0, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	var fired *Suppression
	released, err = governor.ReleaseDue(context.Background(), later, func(_ context.Context, record Suppression) error {
		copy := record
		fired = &copy
		return nil
	})
	if err != nil || released != 1 {
		t.Fatalf("release after recovery: %d %v", released, err)
	}
	if fired == nil || fired.ID != deferredID {
		t.Fatalf("wrong deferred command released: %+v", fired)
	}
	for _, record := range governor.Suppressions(true) {
		if record.ID == deferredID && record.Status != SuppressionRecovered {
			t.Fatalf("released suppression must be recovered, got %s", record.Status)
		}
	}
}

func TestDeferredStartExpiresWhenStale(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 86, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err := governor.GatePumpStart("line-1", "pump-p1", 3, nil, false, now)
	if err != nil {
		t.Fatalf("gate start: %v", err)
	}
	if decision.Suppression == nil {
		t.Fatal("pump start must be deferred")
	}
	later := now.Add(61 * time.Second)
	if _, err := governor.Ingest(reading("blower-b1", 2, 200, 10, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 10, 0, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.ReleaseDue(context.Background(), later, func(context.Context, Suppression) error {
		return ErrDeferredStale
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	for _, record := range governor.Suppressions(true) {
		if record.ID == decision.Suppression.ID && record.Status != SuppressionExpired {
			t.Fatalf("stale deferred command must expire, got %s", record.Status)
		}
	}
}

func TestPumpFlowClamp(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// blower 70 kW, pump 5 kW → available 15, pump affordable 20 kW → 272.7 flow
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 70, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 50, 5, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	decision, err := governor.GatePumpFlow("line-1", "pump-p1", 300, 3, false, now)
	if err != nil {
		t.Fatalf("gate flow: %v", err)
	}
	if decision.Suppression == nil {
		t.Fatal("over-budget flow must be clamped")
	}
	want := 300.0 * 20 / 22
	if decision.Output < want-0.01 || decision.Output > want+0.01 {
		t.Fatalf("clamped flow should be %.2f, got %v", want, decision.Output)
	}
}

func TestUnknownDeviceIsRejected(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.GateBlowerSet("line-1", "blower-unknown", 40, 1, false, now); err == nil {
		t.Fatal("ungoverned device must be rejected")
	}
	if _, err := governor.Ingest(reading("ghost", 1, 1, 1, now), now); err == nil {
		t.Fatal("unregistered meter must be rejected")
	}
	if err := governor.SetBudget("line-9", LineBudget{LimitKW: 100, ReserveKW: 10}); err == nil {
		t.Fatal("unknown line budget must be rejected")
	}
}

func TestReleaseDuePropagatesTransientErrors(t *testing.T) {
	governor := newTestGovernor(t)
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := governor.Ingest(reading("blower-b1", 1, 100, 86, now), now); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.GatePumpStart("line-1", "pump-p1", 3, nil, false, now); err != nil {
		t.Fatalf("gate start: %v", err)
	}
	later := now.Add(61 * time.Second)
	if _, err := governor.Ingest(reading("blower-b1", 2, 200, 10, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := governor.Ingest(reading("pump-p1", 1, 10, 0, later), later); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	boom := errors.New("device bus offline")
	if _, err := governor.ReleaseDue(context.Background(), later, func(context.Context, Suppression) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("transient release errors must propagate, got %v", err)
	}
}

func TestGovernorIsSafeUnderConcurrency(t *testing.T) {
	governor := newTestGovernor(t)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 150; i++ {
				now := base.Add(time.Duration(i%20) * time.Second)
				switch (i + worker) % 4 {
				case 0:
					_, _ = governor.Ingest(reading("blower-b1", uint64(i+1), float64(100+i), 30+float64(i%40), now), now)
				case 1:
					_, _ = governor.GateBlowerSet("line-1", "blower-b1", 48, 1, false, now)
				case 2:
					_, _ = governor.Headroom("line-1", now)
					_ = governor.Status(now, true)
				case 3:
					_ = governor.SetBudget("line-1", LineBudget{LimitKW: 100 + float64(i%5), ReserveKW: 10})
				}
			}
		}(worker)
	}
	wg.Wait()
}
