package process

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/journal"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/power"
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

func (p *Plant) ConfirmHandover(lineID model.LineID, pumpID string, generation uint64, flow float64) (sludge.Handover, *power.Suppression, error) {
	handover, found := p.handovers.Current(lineID)
	if !found {
		return sludge.Handover{}, nil, errors.New("pump handover is not active")
	}
	if handover.Generation != generation || handover.ToPump != pumpID {
		return sludge.Handover{}, nil, errors.New("pump ready confirmation does not belong to current handover")
	}
	clamped, suppression, err := p.gatePumpFlow(lineID, handover.ToPump, flow, generation)
	if err != nil {
		return sludge.Handover{}, nil, err
	}
	confirmed, err := p.handovers.ConfirmReady(lineID, pumpID, generation, clamped, p.now())
	if err != nil {
		return sludge.Handover{}, nil, err
	}
	p.mu.Lock()
	state := p.lines[lineID]
	state.UpdatedAt = p.now().UTC()
	p.lines[lineID] = state
	p.mu.Unlock()
	return confirmed, suppression, nil
}

func (p *Plant) StartSludgeSequence(lineID model.LineID, pumpID string, number uint64) (sludge.Sequence, *power.Suppression, error) {
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok || state.Emergency {
		return sludge.Sequence{}, nil, errors.New("process line cannot start sludge sequence")
	}
	now := p.now()
	decision, err := p.power.GatePumpStart(lineID, pumpID, state.Generation, map[string]string{
		"number": fmt.Sprintf("%d", number),
	}, false, now)
	if err != nil {
		return sludge.Sequence{}, nil, err
	}
	if decision.Suppression != nil {
		// Deferred (错峰): the start fires from ReleaseDeferredPowerCommands
		// once headroom recovers and the stagger window has elapsed.
		return sludge.Sequence{}, decision.Suppression, nil
	}
	sequence, err := p.sequences.Start(lineID, pumpID, number, state.Generation, now)
	if err != nil {
		return sludge.Sequence{}, nil, err
	}
	return sequence, nil, nil
}

func (p *Plant) ConfirmSludgeFlow(lineID model.LineID, sequenceID uuid.UUID, generation uint64, flow float64) (sludge.Sequence, *power.Suppression, error) {
	current, found := p.sequences.Current(lineID)
	if !found || current.ID != sequenceID || current.Generation != generation {
		return sludge.Sequence{}, nil, errors.New("sludge sequence confirmation is stale")
	}
	clamped, suppression, err := p.gatePumpFlow(lineID, current.PumpID, flow, generation)
	if err != nil {
		return sludge.Sequence{}, nil, err
	}
	sequence, err := p.sequences.ConfirmFlow(lineID, sequenceID, generation, clamped, p.now())
	if err != nil {
		return sludge.Sequence{}, nil, err
	}
	return sequence, suppression, nil
}

func (p *Plant) FailSludgeSequence(lineID model.LineID) (sludge.Sequence, error) {
	return p.sequences.Fail(lineID, p.now())
}

func (p *Plant) RunCompensations(ctx context.Context) (int, error) {
	now := p.now()
	return p.compensations.ExecuteDue(ctx, now, func(callCtx context.Context, item journal.Compensation) error {
		return p.sequences.ExecuteCompensation(callCtx, item, now)
	})
}

func (p *Plant) Pumps() []sludge.Pump {
	return p.pumps.Snapshot()
}
