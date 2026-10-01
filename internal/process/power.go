package process

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/aeration"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/power"
)

// releaseGuarded is the single chokepoint through which the power guard
// publishes an allowed command: it lands on the device bus and is applied to
// the matching equipment state.
func (p *Plant) releaseGuarded(ctx context.Context, command model.DeviceCommand) error {
	if err := p.devices.Publish(ctx, command); err != nil {
		return err
	}
	switch command.Kind {
	case model.DeviceBlower:
		if _, err := p.aeration.ApplyCommand(command, p.now()); err != nil {
			return err
		}
	case model.DevicePump:
		if _, err := p.pumps.ApplyCommand(command, p.now()); err != nil {
			return err
		}
	}
	return nil
}

// SubmitAerationGuarded plans a blower target from the DO window and then
// routes it through the power guard. The returned decision tells the operator
// whether the planned command ran as-is, was clamped to the budget, or was
// suppressed. When the window is not yet stable the decision is empty.
func (p *Plant) SubmitAerationGuarded(ctx context.Context, lineID model.LineID, basinID string, payload []byte) (aeration.Window, *power.Decision, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return aeration.Window{}, nil, errors.New("process line cannot accept aeration samples")
	}
	window, planned, err := p.aeration.SubmitWindow(ctx, lineID, basinID, payload, state.Generation, p.now())
	if err != nil {
		return aeration.Window{}, nil, err
	}
	if planned == nil {
		return window, nil, nil
	}
	decision := p.power.Adjudicate(ctx, *planned, false)
	return window, &decision, nil
}

// ManualDeviceCommand routes an operator blower/pump command through the
// budget guard. On an emergency-isolated line the guard is bypassed so safety
// actions (including a full stop) are never limited.
func (p *Plant) ManualDeviceCommand(ctx context.Context, lineID model.LineID, deviceID string, kind model.DeviceKind, setpoint float64) (power.Decision, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok {
		return power.Decision{}, errors.New("process line is not found")
	}
	batchID := uuid.New()
	if active, hasBatch := p.ActiveBatch(lineID); hasBatch {
		batchID = active.ID
	}
	command, err := model.NewDeviceCommand(lineID, batchID, deviceID, kind, state.Generation, "set", p.now())
	if err != nil {
		return power.Decision{}, err
	}
	command.Setpoint = setpoint
	return p.power.Adjudicate(ctx, command, state.Emergency), nil
}

// ReceivePowerReading accepts one meter or live-power telemetry observation.
// A rolled-back meter raises an alarm and never restores trust by itself.
// After a valid reading is stored, deferred commands are retried automatically.
func (p *Plant) ReceivePowerReading(ctx context.Context, reading power.Reading) ([]power.Decision, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, known := p.power.Lookup(reading.DeviceID); !known {
		return nil, errors.New("power reading device is not configured")
	}
	now := p.now()
	if err := p.powerTelemetry.Record(reading, now); err != nil {
		if reading.Source == power.SourceMeter {
			p.alarms.Raise(model.NewAlarm(p.deviceLine(reading.DeviceID), "POWER_METER_ROLLBACK",
				"energy meter reading rolled back for "+reading.DeviceID+"; power headroom is untrusted",
				model.SeverityWarning, now))
		}
		return nil, err
	}
	return p.power.Reevaluate(ctx), nil
}

// ResynchronisePower clears a sticky rollback flag after a confirmed meter
// reset, establishing a fresh baseline.
func (p *Plant) ResynchronisePower(deviceID string) error {
	if _, known := p.power.Lookup(deviceID); !known {
		return errors.New("power reading device is not configured")
	}
	return p.powerTelemetry.Resynchronise(deviceID, p.now())
}

func (p *Plant) deviceLine(deviceID string) model.LineID {
	if profile, ok := p.power.Lookup(deviceID); ok {
		return profile.LineID
	}
	return ""
}

// PowerHeadroom returns the per-line budget picture.
func (p *Plant) PowerHeadroom() []power.LineHeadroom {
	return p.power.Snapshot()
}

// PowerSuppressed returns the commands currently withheld and their recovery
// conditions.
func (p *Plant) PowerSuppressed() []power.SuppressedCommand {
	return p.power.Suppressed()
}

// PowerHistory returns the most recent guard verdicts.
func (p *Plant) PowerHistory() []power.Decision {
	return p.power.History()
}
