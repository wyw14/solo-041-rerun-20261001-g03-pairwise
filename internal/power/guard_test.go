package power

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

const testLine model.LineID = "line-1"

func testHarness(t *testing.T, capacity float64, now time.Time) (*Manager, *TelemetryBook, []Profile) {
	t.Helper()
	profiles := []Profile{
		{ID: "blower-basin-line-1", LineID: testLine, Kind: model.DeviceBlower, Role: RoleBlower,
			RatedPowerW: 55000, MinPowerW: 21000, MinSetpoint: 28, MaxSetpoint: 60},
		{ID: "pump-line-1-A", LineID: testLine, Kind: model.DevicePump, Role: RoleReturnPump,
			RatedPowerW: 18500, MinPowerW: 18500, MinSetpoint: 1, MaxSetpoint: 1},
		{ID: "pump-line-1-B", LineID: testLine, Kind: model.DevicePump, Role: RoleReturnPump,
			RatedPowerW: 18500, MinPowerW: 18500, MinSetpoint: 1, MaxSetpoint: 1},
	}
	registry, err := NewRegistry(profiles)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	budgets, err := NewBudgetBook([]LineBudget{{LineID: testLine, CapacityW: capacity}})
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	telemetry := NewTelemetryBook(time.Minute)
	calculator, err := NewCalculator(budgets, registry, telemetry)
	if err != nil {
		t.Fatalf("calculator: %v", err)
	}
	clock := now
	manager, err := NewManager(calculator, registry, telemetry, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("manager: %v", err)
	}
	published := make([]model.DeviceCommand, 0)
	manager.BindReleaser(func(_ context.Context, command model.DeviceCommand) error {
		published = append(published, command)
		return nil
	})
	t.Cleanup(func() {})
	return manager, telemetry, profiles
}

func powerReading(device string, watts float64, at time.Time) Reading {
	return Reading{DeviceID: device, Source: SourcePower, PowerW: watts, ObservedAt: at}
}

func meterReading(device string, kwh float64, at time.Time) Reading {
	return Reading{DeviceID: device, Source: SourceMeter, EnergyKWh: kwh, ObservedAt: at}
}

func blowerCommand(setpoint float64, at time.Time) model.DeviceCommand {
	command, _ := model.NewDeviceCommand(testLine, uuid.New(), "blower-basin-line-1", model.DeviceBlower, 1, "set", at)
	command.Setpoint = setpoint
	return command
}

func pumpCommand(device string, setpoint float64, at time.Time) model.DeviceCommand {
	command, _ := model.NewDeviceCommand(testLine, uuid.New(), device, model.DevicePump, 1, "set", at)
	command.Setpoint = setpoint
	return command
}

// With no readings at all the line is untrusted: headroom must be zero rather
// than optimistically "full capacity".
func TestNoReadingsMeansNoHeadroom(t *testing.T) {
	now := time.Now().UTC()
	manager, _, profiles := testHarness(t, 75000, now)
	heads := manager.Snapshot()
	if len(heads) != 1 {
		t.Fatalf("expected one headroom, got %d", len(heads))
	}
	if heads[0].Trusted {
		t.Fatalf("line with no readings must not be trusted")
	}
	if heads[0].HeadroomW != 0 {
		t.Fatalf("untrusted line must report zero headroom, got %.0f", heads[0].HeadroomW)
	}
	if heads[0].ReservedW != profiles[0].MinPowerW+profiles[1].MinPowerW {
		t.Fatalf("reserved must protect minimum blower + one pump, got %.0f", heads[0].ReservedW)
	}
}

// A stale reading must collapse trust and headroom.
func TestStaleReadingCollapsesHeadroom(t *testing.T) {
	start := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, start)
	if err := telemetry.Record(powerReading("blower-basin-line-1", 21000, start.Add(-2*time.Minute)), start); err != nil {
		t.Fatalf("record: %v", err)
	}
	head, _ := manager.calculator.Line(testLine, start)
	if head.Trusted {
		t.Fatalf("two-minute-old reading with 1m staleness must be stale")
	}
	if head.HeadroomW != 0 {
		t.Fatalf("stale line must report zero headroom, got %.0f", head.HeadroomW)
	}
}

// A backwards-jumping energy meter is flagged and never offers headroom until
// an explicit resynchronisation.
func TestMeterRollbackRequiresResync(t *testing.T) {
	start := time.Now().UTC()
	_, telemetry, _ := testHarness(t, 75000, start)
	device := "blower-basin-line-1"
	if err := telemetry.Record(meterReading(device, 10.0, start), start); err != nil {
		t.Fatalf("first meter reading: %v", err)
	}
	if err := telemetry.Record(meterReading(device, 9.5, start.Add(time.Second)), start.Add(time.Second)); err == nil {
		t.Fatalf("rolled-back meter must be rejected")
	}
	status := telemetry.DeviceStatus(device, start.Add(time.Second))
	if status.Problem != ProblemRollback || status.Trusted {
		t.Fatalf("expected sticky rollback, got %+v", status)
	}
	// A later monotonic sample must NOT launder trust.
	if err := telemetry.Record(meterReading(device, 10.1, start.Add(2*time.Second)), start.Add(2*time.Second)); err != nil {
		t.Fatalf("post-rollback monotonic reading accepted: %v", err)
	}
	if status = telemetry.DeviceStatus(device, start.Add(2*time.Second)); status.Trusted {
		t.Fatalf("rollback must persist until explicit resync")
	}
	if err := telemetry.Resynchronise(device, start.Add(3*time.Second)); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if err := telemetry.Record(meterReading(device, 10.2, start.Add(4*time.Second)), start.Add(4*time.Second)); err != nil {
		t.Fatalf("post-resync reading: %v", err)
	}
	if status = telemetry.DeviceStatus(device, start.Add(4*time.Second)); !status.Trusted {
		t.Fatalf("expected trusted after resync, got %+v", status)
	}
}

// Minimum aeration: an off / below-floor blower command is rejected.
func TestMinimumAerationProtected(t *testing.T) {
	now := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, now)
	if err := telemetry.Record(powerReading("blower-basin-line-1", 21000, now), now); err != nil {
		t.Fatalf("seed reading: %v", err)
	}
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 0, now), now)
	stop := blowerCommand(0, now)
	if decision := manager.Adjudicate(context.Background(), stop, false); decision.Outcome != OutcomeRejected || decision.Reason != ReasonMinAeration {
		t.Fatalf("blower stop should be rejected, got %s %s", decision.Outcome, decision.Reason)
	}
	low := blowerCommand(20, now)
	if decision := manager.Adjudicate(context.Background(), low, false); decision.Outcome != OutcomeRejected {
		t.Fatalf("below-floor frequency should be rejected, got %s", decision.Outcome)
	}
}

// Minimum return: the only running pump cannot be stopped, but it can once the
// standby pump is running.
func TestMinimumReturnProtected(t *testing.T) {
	now := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, now)
	telemetry.Record(powerReading("blower-basin-line-1", 21000, now), now)
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 0, now), now)
	manager.MarkRunning("pump-line-1-A", 1)
	stopA := pumpCommand("pump-line-1-A", 0, now)
	if decision := manager.Adjudicate(context.Background(), stopA, false); decision.Outcome != OutcomeRejected || decision.Reason != ReasonMinReturn {
		t.Fatalf("stopping the only return pump should be rejected, got %s %s", decision.Outcome, decision.Reason)
	}
	// Start the standby pump (plenty of budget at 1 kW measured).
	startB := pumpCommand("pump-line-1-B", 1, now)
	if decision := manager.Adjudicate(context.Background(), startB, false); decision.Outcome != OutcomeAllowed {
		t.Fatalf("standby pump start should fit, got %s (%s)", decision.Outcome, decision.Reason)
	}
	telemetry.Record(powerReading("pump-line-1-B", 18500, now), now)
	// Now A can stop because B preserves the minimum return.
	if decision := manager.Adjudicate(context.Background(), stopA, false); decision.Outcome != OutcomeAllowed {
		t.Fatalf("pump A may stop once B carries return, got %s (%s)", decision.Outcome, decision.Reason)
	}
}

// Over-budget blower increase is clamped to the remaining power and never
// below the aeration floor; the published command carries the clamp.
func TestBlowerClampedToBudget(t *testing.T) {
	now := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 65000, now)
	blower := "blower-basin-line-1"
	// Blower measured at 34.6 kW plus pump A (18.5 kW) = 53.1 kW; a jump to
	// 55 Hz (~49.7 kW) would total ~68.2 kW against the 65 kW contract.
	telemetry.Record(powerReading(blower, 34600, now), now)
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 0, now), now)
	manager.MarkRunning(blower, 36)

	decision := manager.Adjudicate(context.Background(), blowerCommand(55, now), false)
	if decision.Outcome != OutcomeClamped {
		t.Fatalf("expected clamp, got %s (%s)", decision.Outcome, decision.Reason)
	}
	if decision.FinalSetpoint <= 36 || decision.FinalSetpoint < 28 {
		t.Fatalf("clamp must stay above running 36 Hz and at/above floor, got %.2f", decision.FinalSetpoint)
	}
	if decision.Command.Setpoint != decision.FinalSetpoint {
		t.Fatalf("published command must carry clamped setpoint")
	}
	profile, _ := manager.Lookup(blower)
	post := 18500 + profile.PowerW(decision.FinalSetpoint)
	if post > 65000+1 {
		t.Fatalf("clamped plan still exceeds budget: %.0f W", post)
	}
}

// A second return pump that would breach the contract is deferred (peak
// shaving), then auto-released when the running blower frees power.
func TestPumpDeferredThenReleased(t *testing.T) {
	now := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, now)
	blower := "blower-basin-line-1"
	// 55 kW blower + 18.5 kW pump A = 73.5 kW; pump B (+18.5) would breach.
	telemetry.Record(powerReading(blower, 55000, now), now)
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 0, now), now)
	manager.MarkRunning(blower, 60)
	manager.MarkRunning("pump-line-1-A", 1)

	decision := manager.Adjudicate(context.Background(), pumpCommand("pump-line-1-B", 1, now), false)
	if decision.Outcome != OutcomeDeferred || decision.Reason != ReasonPeakShaving {
		t.Fatalf("second pump should be deferred, got %s (%s)", decision.Outcome, decision.Reason)
	}
	if len(decision.Recovery) == 0 {
		t.Fatalf("deferred command must carry recovery conditions")
	}
	if suppressed := manager.Suppressed(); len(suppressed) != 1 {
		t.Fatalf("expected one suppressed command, got %d", len(suppressed))
	}

	// Free power by turning the blower down (a reduction is always allowed).
	manager.Adjudicate(context.Background(), blowerCommand(28, now), false)
	telemetry.Record(powerReading(blower, 21000, now.Add(time.Second)), now.Add(time.Second))

	released := manager.Reevaluate(context.Background())
	if len(released) != 1 || released[0].Outcome != OutcomeAllowed {
		t.Fatalf("expected the deferred pump to auto-release once power freed, got %+v", released)
	}
	if suppressed := manager.Suppressed(); len(suppressed) != 0 {
		t.Fatalf("suppressed queue should drain after release")
	}
}

// While readings are untrusted an above-floor increase is deferred and does NOT
// release on Reevaluate; it releases after a fresh trusted reading frees room.
func TestUntrustedReadingsDeferIncrease(t *testing.T) {
	start := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, start)
	blower := "blower-basin-line-1"
	// Only a stale blower reading exists; pumps never reported.
	telemetry.Record(powerReading(blower, 30000, start.Add(-2*time.Minute)), start.Add(-2*time.Minute))

	// A high blower increase must be parked, not guessed.
	decision := manager.Adjudicate(context.Background(), blowerCommand(50, start), false)
	if decision.Outcome != OutcomeDeferred || decision.Reason != ReasonReadingsUntrust {
		t.Fatalf("untrusted increase should defer, got %s (%s)", decision.Outcome, decision.Reason)
	}
	// Re-evaluation while stale must not release.
	if released := manager.Reevaluate(context.Background()); len(released) != 0 {
		t.Fatalf("stale readings must not release deferred commands")
	}
	// First pump (protected minimum) can still be established blind.
	pumpB := manager.Adjudicate(context.Background(), pumpCommand("pump-line-1-A", 1, start), false)
	if pumpB.Outcome != OutcomeAllowed {
		t.Fatalf("first return pump (minimum return) should run blind, got %s (%s)", pumpB.Outcome, pumpB.Reason)
	}
	// Second pump cannot run blind.
	second := manager.Adjudicate(context.Background(), pumpCommand("pump-line-1-B", 1, start), false)
	if second.Outcome != OutcomeDeferred {
		t.Fatalf("second pump must wait for trusted readings, got %s", second.Outcome)
	}

	// Fresh, low readings restore trust and release the queued blower command.
	now := start.Add(time.Second)
	telemetry.Record(powerReading(blower, 21000, now), now)
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 0, now), now)
	released := manager.Reevaluate(context.Background())
	found := false
	for _, item := range released {
		if item.Command.DeviceID == blower {
			found = true
		}
	}
	if !found {
		t.Fatalf("blower command should auto-release once readings trusted and room exists: %+v", released)
	}
}

// Emergency commands bypass every limit, including minimum protection and the
// contract ceiling, and are recorded as such.
func TestEmergencyBypass(t *testing.T) {
	now := time.Now().UTC()
	manager, telemetry, _ := testHarness(t, 75000, now)
	telemetry.Record(powerReading("blower-basin-line-1", 60000, now), now)
	telemetry.Record(powerReading("pump-line-1-A", 18500, now), now)
	telemetry.Record(powerReading("pump-line-1-B", 18500, now), now)

	decision := manager.Adjudicate(context.Background(), blowerCommand(0, now), true)
	if decision.Outcome != OutcomeBypassed || decision.FinalSetpoint != 0 {
		t.Fatalf("emergency blower stop must bypass, got %s", decision.Outcome)
	}
}

// A zero-watt live reading for a stopped device must overwrite a previously
// measured load, otherwise the budget would keep charging a pump that stopped.
func TestZeroPowerReadingClearsLoad(t *testing.T) {
	now := time.Now().UTC()
	_, telemetry, _ := testHarness(t, 75000, now)
	device := "pump-line-1-A"
	if err := telemetry.Record(powerReading(device, 18500, now), now); err != nil {
		t.Fatalf("running reading: %v", err)
	}
	if status := telemetry.DeviceStatus(device, now); status.PowerW != 18500 {
		t.Fatalf("expected 18500 W, got %.0f", status.PowerW)
	}
	if err := telemetry.Record(powerReading(device, 0, now.Add(time.Second)), now.Add(time.Second)); err != nil {
		t.Fatalf("stopped reading: %v", err)
	}
	if status := telemetry.DeviceStatus(device, now.Add(time.Second)); status.PowerW != 0 || !status.Trusted {
		t.Fatalf("stopped device must read 0 and stay trusted, got %+v", status)
	}
}

// Profile power mapping and its inverse agree within tolerance.
func TestSetpointForPowerRoundTrip(t *testing.T) {
	profile := Profile{RatedPowerW: 55000, MinPowerW: 21000, MinSetpoint: 28, MaxSetpoint: 60}
	for _, target := range []float64{28, 36, 45, 60} {
		watts := profile.PowerW(target)
		back := profile.SetpointForPower(watts)
		diff := back - target
		if diff < -0.01 || diff > 0.01 {
			t.Fatalf("round-trip mismatch at %.1f: %.3f (%.0f W)", target, back, watts)
		}
	}
	if got := profile.SetpointForPower(-100); got != 28 {
		t.Fatalf("negative budget must clamp to floor, got %.2f", got)
	}
	if got := profile.SetpointForPower(999999); got != 60 {
		t.Fatalf("excess budget must clamp to ceiling, got %.2f", got)
	}
}
