package process

import (
	"context"
	"errors"

	"github.com/wyw14/cry-97/internal/model"
)

type EmergencyRecord struct {
	LineID     model.LineID `json:"line_id"`
	Reason     string       `json:"reason"`
	Generation uint64       `json:"generation"`
}

func (p *Plant) EmergencyStop(ctx context.Context, lineID model.LineID, reason string) (model.LineState, error) {
	if reason == "" {
		return model.LineState{}, errors.New("emergency reason is required")
	}
	p.mu.RLock()
	state, ok := p.lines[lineID]
	p.mu.RUnlock()
	if !ok {
		return model.LineState{}, errors.New("process line is not found")
	}
	now := p.now()
	record := EmergencyRecord{LineID: lineID, Reason: reason, Generation: state.Generation + 1}
	event, err := model.NewEvent(lineID, record.Generation, model.EventEmergencyIsolated, record, now)
	if err != nil {
		return model.LineState{}, err
	}
	persisted, err := p.store.Append(ctx, event)
	if err != nil {
		return model.LineState{}, err
	}
	p.mu.Lock()
	state = p.lines[lineID].Isolate(persisted.Sequence, now)
	p.lines[lineID] = state
	p.mu.Unlock()
	p.devices.StopLine(lineID, state.Generation)
	p.power.CancelLine(lineID)
	p.equalizers.Release(lineID)
	p.alarms.Raise(model.NewAlarm(lineID, "PROCESS_EMERGENCY", reason, model.SeverityCritical, now))
	if err := p.snapshots.Save(state); err != nil {
		return model.LineState{}, err
	}
	return state, nil
}
