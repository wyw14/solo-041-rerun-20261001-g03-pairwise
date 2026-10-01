package power

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
)

// Config tunes the governor. Limits are per process line.
type Config struct {
	LimitKW       float64
	ReserveKW     float64
	StaleAfter    time.Duration
	StaggerWindow time.Duration
	MaxDefer      time.Duration
	WrapTolerance float64
}

func (c Config) validate() error {
	if c.LimitKW <= 0 || c.ReserveKW < 0 || c.ReserveKW >= c.LimitKW {
		return errors.New("power limit and reserve are invalid")
	}
	if c.StaleAfter <= 0 || c.StaggerWindow <= 0 || c.MaxDefer < c.StaggerWindow {
		return errors.New("power telemetry and stagger windows are invalid")
	}
	if c.WrapTolerance < 0 {
		return errors.New("power wrap tolerance is invalid")
	}
	return nil
}

// Decision is the outcome of gating one command: the output that may be
// applied now, plus the suppression record when the request was held back.
type Decision struct {
	Output      float64
	Suppression *Suppression
}

// ErrDeferredStale marks a deferred command whose context (e.g. line
// generation) no longer matches; the governor expires it instead of retrying.
var ErrDeferredStale = errors.New("deferred power command is stale")

// Governor clamps or staggers new device commands so a process line stays
// inside its power budget. Minimum aeration and return-flow floors are
// protected: they are never clamped away, and stop commands always pass.
type Governor struct {
	mu           sync.RWMutex
	meters       *MeterBook
	devices      map[string]DeviceProfile
	lines        map[model.LineID][]string
	budgets      map[model.LineID]LineBudget
	stagger      time.Duration
	maxDefer     time.Duration
	suppressions *suppressionBook
}

func NewGovernor(devices []DeviceProfile, budget LineBudget, cfg Config) (*Governor, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := budget.validate(); err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, errors.New("power governor requires governed devices")
	}
	governor := &Governor{
		devices:      make(map[string]DeviceProfile, len(devices)),
		lines:        make(map[model.LineID][]string),
		budgets:      make(map[model.LineID]LineBudget),
		stagger:      cfg.StaggerWindow,
		maxDefer:     cfg.MaxDefer,
		suppressions: newSuppressionBook(),
	}
	for _, device := range devices {
		if device.DeviceID == "" || device.LineID == "" {
			return nil, errors.New("governed device identity is incomplete")
		}
		if device.NameplateKW <= 0 || device.RatedOutput <= 0 || device.MinOutput < 0 || device.MinOutput > device.RatedOutput {
			return nil, fmt.Errorf("governed device %s has an invalid load model", device.DeviceID)
		}
		if _, exists := governor.devices[device.DeviceID]; exists {
			return nil, fmt.Errorf("governed device %s is duplicated", device.DeviceID)
		}
		governor.devices[device.DeviceID] = device
		governor.lines[device.LineID] = append(governor.lines[device.LineID], device.DeviceID)
	}
	meters, err := newMeterBook(devices, cfg.StaleAfter, cfg.WrapTolerance)
	if err != nil {
		return nil, err
	}
	governor.meters = meters
	for lineID := range governor.lines {
		governor.budgets[lineID] = budget
	}
	return governor, nil
}

// Ingest stores one meter reading; stale or rolled-over meters shrink the
// reported headroom instead of blocking commands outright.
func (g *Governor) Ingest(reading Reading, now time.Time) (Meter, error) {
	return g.meters.Ingest(reading, now)
}

// Headroom reports the line's remaining budget. Devices without trustworthy
// telemetry are accounted at nameplate draw, so the value is never optimistic.
func (g *Governor) Headroom(lineID model.LineID, now time.Time) (HeadroomView, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	deviceIDs, ok := g.lines[lineID]
	if !ok {
		return HeadroomView{}, fmt.Errorf("power budget for line %s is not configured", lineID)
	}
	devices := make([]DeviceProfile, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		devices = append(devices, g.devices[deviceID])
	}
	return headroom(lineID, g.budgets[lineID], devices, g.meters, now), nil
}

// SetBudget replaces a line's budget envelope at runtime.
func (g *Governor) SetBudget(lineID model.LineID, budget LineBudget) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.lines[lineID]; !ok {
		return fmt.Errorf("power budget for line %s is not configured", lineID)
	}
	if err := budget.validate(); err != nil {
		return err
	}
	g.budgets[lineID] = budget
	return nil
}

// GateBlowerSet clamps a blower frequency setpoint (Hz) to the affordable
// headroom. A setpoint of zero (stop) and the minimum-aeration floor always
// pass; emergency lines bypass gating entirely.
func (g *Governor) GateBlowerSet(lineID model.LineID, deviceID string, targetHz float64, generation uint64, emergency bool, now time.Time) (Decision, error) {
	return g.gateOutput(lineID, deviceID, "set", targetHz, generation, emergency, now)
}

// GatePumpFlow clamps a return-pump flow setpoint to the affordable headroom,
// never below the minimum return-flow floor.
func (g *Governor) GatePumpFlow(lineID model.LineID, deviceID string, flow float64, generation uint64, emergency bool, now time.Time) (Decision, error) {
	return g.gateOutput(lineID, deviceID, "flow", flow, generation, emergency, now)
}

func (g *Governor) gateOutput(lineID model.LineID, deviceID, action string, requested float64, generation uint64, emergency bool, now time.Time) (Decision, error) {
	profile, ok := g.devices[deviceID]
	if !ok || profile.LineID != lineID {
		return Decision{}, fmt.Errorf("device %s is not governed on line %s", deviceID, lineID)
	}
	if requested <= 0 || emergency {
		// Load-shedding commands and emergency handling are never held back.
		return Decision{Output: requested}, nil
	}
	view, err := g.Headroom(lineID, now)
	if err != nil {
		return Decision{}, err
	}
	accounted, err := deviceDrawOf(view, deviceID)
	if err != nil {
		return Decision{}, err
	}
	affordable := view.AvailableKW + accounted
	requestedKW := profile.KWForOutput(requested)
	if requestedKW <= affordable {
		return Decision{Output: requested}, nil
	}
	output := profile.OutputForKW(affordable)
	if output < profile.MinOutput {
		// Minimum aeration / return flow is a protection floor: it is exempt
		// from clamping even when the budget cannot cover it.
		output = profile.MinOutput
	}
	if output >= requested {
		return Decision{Output: requested}, nil
	}
	neededKW := requestedKW - accounted
	record := g.suppressions.add(Suppression{
		ID: uuid.New(), LineID: lineID, DeviceID: deviceID, Kind: SuppressionClamped,
		Action: action, Generation: generation, RequestedOutput: requested, AppliedOutput: output,
		NeededKW: neededKW, Reason: "线路功率余量不足，已限幅",
		Recovery:  fmt.Sprintf("线路可用余量恢复至 ≥ %.1f kW（当前 %.1f kW）", neededKW, view.AvailableKW),
		CreatedAt: now.UTC(), Status: SuppressionActive,
	})
	return Decision{Output: output, Suppression: &record}, nil
}

// GatePumpStart defers a pump start when the line cannot cover even the
// pump's minimum draw right now (错峰). The deferred command becomes
// eligible once headroom recovers and the stagger window has elapsed.
func (g *Governor) GatePumpStart(lineID model.LineID, deviceID string, generation uint64, payload map[string]string, emergency bool, now time.Time) (Decision, error) {
	profile, ok := g.devices[deviceID]
	if !ok || profile.LineID != lineID {
		return Decision{}, fmt.Errorf("device %s is not governed on line %s", deviceID, lineID)
	}
	if emergency {
		return Decision{}, nil
	}
	view, err := g.Headroom(lineID, now)
	if err != nil {
		return Decision{}, err
	}
	neededKW := profile.MinKW()
	if view.AvailableKW >= neededKW {
		return Decision{}, nil
	}
	notBefore := now.UTC().Add(g.stagger)
	record := g.suppressions.add(Suppression{
		ID: uuid.New(), LineID: lineID, DeviceID: deviceID, Kind: SuppressionDeferred,
		Action: "start", Generation: generation, NeededKW: neededKW,
		Reason:   "线路功率余量不足，泵启动已错峰",
		Recovery: fmt.Sprintf("线路可用余量 ≥ %.1f kW 且不早于 %s", neededKW, notBefore.Format("15:04:05")),
		Payload:  payload, NotBefore: notBefore, ExpiresAt: now.UTC().Add(g.maxDefer),
		CreatedAt: now.UTC(), Status: SuppressionActive,
	})
	return Decision{Suppression: &record}, nil
}

// ReleaseDue fires deferred commands whose stagger window elapsed and whose
// headroom recovered, expires the ones that waited too long, and marks
// clamped records recovered once their budget condition holds again.
func (g *Governor) ReleaseDue(ctx context.Context, now time.Time, release func(context.Context, Suppression) error) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	released := 0
	for _, record := range g.suppressions.activeDeferred(now) {
		if now.Before(record.NotBefore) {
			continue
		}
		if !record.ExpiresAt.IsZero() && now.After(record.ExpiresAt) {
			g.suppressions.resolve(record.ID, SuppressionExpired, now)
			continue
		}
		view, err := g.Headroom(record.LineID, now)
		if err != nil || view.AvailableKW < record.NeededKW {
			continue
		}
		if err := release(ctx, record); err != nil {
			if errors.Is(err, ErrDeferredStale) {
				g.suppressions.resolve(record.ID, SuppressionExpired, now)
				continue
			}
			return released, err
		}
		g.suppressions.resolve(record.ID, SuppressionRecovered, now)
		released++
	}
	for _, record := range g.suppressions.activeClamped() {
		view, err := g.Headroom(record.LineID, now)
		if err != nil {
			continue
		}
		if view.AvailableKW >= record.NeededKW {
			g.suppressions.resolve(record.ID, SuppressionRecovered, now)
		}
	}
	return released, nil
}

// Status is the operator-facing snapshot of budgets, meters and suppressions.
type Status struct {
	Lines        []HeadroomView `json:"lines"`
	Meters       []MeterView    `json:"meters"`
	Suppressions []Suppression  `json:"suppressions"`
	ObservedAt   time.Time      `json:"observed_at"`
}

func (g *Governor) Status(now time.Time, includeResolved bool) Status {
	status := Status{
		Lines: make([]HeadroomView, 0, len(g.lines)), Meters: g.meters.Views(now),
		Suppressions: g.suppressions.list(includeResolved), ObservedAt: now.UTC(),
	}
	for lineID := range g.lines {
		if view, err := g.Headroom(lineID, now); err == nil {
			status.Lines = append(status.Lines, view)
		}
	}
	sort.Slice(status.Lines, func(i, j int) bool { return status.Lines[i].LineID < status.Lines[j].LineID })
	return status
}

// Suppressions lists suppressed commands, newest first.
func (g *Governor) Suppressions(includeResolved bool) []Suppression {
	return g.suppressions.list(includeResolved)
}
