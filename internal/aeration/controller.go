package aeration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/sensor"
)

type Controller struct {
	decoder *sensor.BatchDecoder
	windows *WindowBook
	blowers *BlowerFleet
}

func NewController(decoder *sensor.BatchDecoder, windows *WindowBook, blowers *BlowerFleet) (*Controller, error) {
	if decoder == nil || windows == nil || blowers == nil {
		return nil, errors.New("aeration controller dependencies are required")
	}
	return &Controller{decoder: decoder, windows: windows, blowers: blowers}, nil
}

func (c *Controller) SubmitWindow(ctx context.Context, lineID model.LineID, basinID string, payload []byte, generation uint64, now time.Time) (Window, *model.DeviceCommand, error) {
	if err := ctx.Err(); err != nil {
		return Window{}, nil, err
	}
	values, err := c.decoder.Decode(payload)
	if err != nil {
		return Window{}, nil, err
	}
	window, err := c.windows.Store(basinID, values)
	if err != nil {
		return Window{}, nil, err
	}
	if !window.Stable {
		return window, nil, nil
	}
	target := 36.0
	if window.Average < 2.0 {
		target = 48
	} else if window.Average > 3.5 {
		target = 28
	}
	command, err := model.NewDeviceCommand(lineID, uuid.New(), "blower-"+basinID, model.DeviceBlower, generation, "set", now)
	if err != nil {
		return Window{}, nil, err
	}
	command.Setpoint = target
	// The caller routes the planned command through the power-budget guard and
	// applies the (possibly clamped) result with ApplyCommand.
	return window, &command, nil
}

// ApplyCommand commits a guarded blower command to the fleet state.
func (c *Controller) ApplyCommand(command model.DeviceCommand, now time.Time) (Blower, error) {
	return c.blowers.Apply(command, now)
}

func (c *Controller) Windows() []Window {
	return c.windows.All()
}

func (c *Controller) Blowers() []Blower {
	return c.blowers.Snapshot()
}
