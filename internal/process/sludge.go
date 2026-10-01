package process

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/sludge"
)

func (p *Plant) StartHandover(lineID model.LineID, fromPump, toPump string) (sludge.Handover, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return sludge.Handover{}, errors.New("process line cannot start pump handover")
	}
	return p.handovers.Start(lineID, fromPump, toPump, state.Generation, p.now())
}

func (p *Plant) ConfirmHandover(lineID model.LineID, pumpID string, generation uint64, flow float64) (sludge.Handover, error) {
	handover, err := p.handovers.ConfirmReady(lineID, pumpID, generation, flow, p.now())
	if err != nil {
		return sludge.Handover{}, err
	}
	p.mu.Lock()
	state := p.lines[lineID]
	state.UpdatedAt = p.now().UTC()
	p.lines[lineID] = state
	p.mu.Unlock()
	return handover, nil
}

func (p *Plant) StartSludgeSequence(lineID model.LineID, pumpID string, number uint64) (sludge.Sequence, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return sludge.Sequence{}, errors.New("process line cannot start sludge sequence")
	}
	return p.sequences.Start(lineID, pumpID, number, state.Generation, p.now())
}

func (p *Plant) ConfirmSludgeFlow(lineID model.LineID, sequenceID uuid.UUID, generation uint64, flow float64) (sludge.Sequence, error) {
	sequence, err := p.sequences.ConfirmFlow(lineID, sequenceID, generation, flow, p.now())
	if err != nil {
		return sludge.Sequence{}, err
	}
	// Reflect the confirmed running pump so the budget guard knows the minimum
	// return requirement is now held by this device.
	p.power.MarkRunning(sequence.PumpID, 1)
	return sequence, nil
}

func (p *Plant) FailSludgeSequence(lineID model.LineID) (sludge.Sequence, error) {
	return p.sequences.Fail(lineID, p.now())
}

func (p *Plant) RunCompensations(ctx context.Context) (int, error) {
	now := p.now()
	executed, err := p.compensations.ExecuteDue(ctx, now, func(callCtx context.Context, item journal.Compensation) error {
		if execErr := p.sequences.ExecuteCompensation(callCtx, item, now); execErr != nil {
			return execErr
		}
		p.power.MarkRunning(item.DeviceID, 0)
		return nil
	})
	if err != nil {
		return executed, err
	}
	// A pump stopping may free enough power to release a staggered command.
	_ = p.power.Reevaluate(ctx)
	return executed, nil
}

func (p *Plant) Pumps() []sludge.Pump {
	return p.pumps.Snapshot()
}
