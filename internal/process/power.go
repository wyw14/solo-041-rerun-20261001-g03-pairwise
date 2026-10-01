package process

import (
	"context"
	"strconv"

	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/power"
)

// ReceivePowerReading ingests one device meter reading, then gives deferred
// commands a chance to fire: fresh telemetry may restore the headroom they
// were waiting for.
func (p *Plant) ReceivePowerReading(ctx context.Context, reading power.Reading) (power.Meter, error) {
	meter, err := p.power.Ingest(reading, p.now())
	if err != nil {
		return power.Meter{}, err
	}
	if _, err := p.ReleaseDeferredPowerCommands(ctx); err != nil {
		return meter, err
	}
	return meter, nil
}

// ReleaseDeferredPowerCommands fires deferred (错峰) commands whose stagger
// window elapsed and whose headroom recovered. Deferred pump starts re-enter
// through the normal sequence path so ownership and generation checks still
// apply; a line that moved on (new generation, emergency) voids the deferral.
func (p *Plant) ReleaseDeferredPowerCommands(ctx context.Context) (int, error) {
	now := p.now()
	return p.power.ReleaseDue(ctx, now, func(callCtx context.Context, record power.Suppression) error {
		if err := callCtx.Err(); err != nil {
			return err
		}
		if record.Action != "start" {
			return power.ErrDeferredStale
		}
		p.mu.RLock()
		state, ok := p.lines[record.LineID]
		p.mu.RUnlock()
		if !ok || state.Emergency || state.Generation != record.Generation {
			return power.ErrDeferredStale
		}
		number, err := strconv.ParseUint(record.Payload["number"], 10, 64)
		if err != nil || number == 0 {
			return power.ErrDeferredStale
		}
		_, startErr := p.sequences.Start(record.LineID, record.DeviceID, number, record.Generation, now)
		return startErr
	})
}

func (p *Plant) PowerStatus() power.Status {
	return p.power.Status(p.now(), true)
}

func (p *Plant) PowerSuppressions() []power.Suppression {
	return p.power.Suppressions(false)
}

func (p *Plant) SetLinePowerBudget(lineID model.LineID, budget power.LineBudget) error {
	return p.power.SetBudget(lineID, budget)
}

// gatePumpFlow clamps a pump flow setpoint to the line's remaining budget.
// The line must be known and not emergency-isolated (checked by callers).
func (p *Plant) gatePumpFlow(lineID model.LineID, pumpID string, flow float64, generation uint64) (float64, *power.Suppression, error) {
	decision, err := p.power.GatePumpFlow(lineID, pumpID, flow, generation, false, p.now())
	if err != nil {
		return 0, nil, err
	}
	return decision.Output, decision.Suppression, nil
}
