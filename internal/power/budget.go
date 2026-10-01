package power

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

// DeviceProfile describes one governed device's electrical model. Draw scales
// linearly with output (blower Hz, pump flow) up to the nameplate rating.
type DeviceProfile struct {
	DeviceID    string           `json:"device_id"`
	LineID      model.LineID     `json:"line_id"`
	Kind        model.DeviceKind `json:"kind"`
	NameplateKW float64          `json:"nameplate_kw"`
	RatedOutput float64          `json:"rated_output"`
	MinOutput   float64          `json:"min_output"`
}

// KWForOutput estimates the draw at a given output setpoint.
func (d DeviceProfile) KWForOutput(output float64) float64 {
	if output <= 0 || d.RatedOutput <= 0 {
		return 0
	}
	if output > d.RatedOutput {
		output = d.RatedOutput
	}
	return d.NameplateKW * output / d.RatedOutput
}

// OutputForKW inverts the load model: the largest output affordable with kw.
func (d DeviceProfile) OutputForKW(kw float64) float64 {
	if kw <= 0 || d.NameplateKW <= 0 {
		return 0
	}
	output := d.RatedOutput * kw / d.NameplateKW
	if output > d.RatedOutput {
		return d.RatedOutput
	}
	return output
}

// MinKW is the protected minimum draw (minimum aeration / return flow).
func (d DeviceProfile) MinKW() float64 {
	return d.KWForOutput(d.MinOutput)
}

// LineBudget is the contracted power envelope for one process line.
type LineBudget struct {
	LimitKW   float64 `json:"limit_kw"`
	ReserveKW float64 `json:"reserve_kw"`
}

func (b LineBudget) validate() error {
	if b.LimitKW <= 0 || b.ReserveKW < 0 || b.ReserveKW >= b.LimitKW {
		return errors.New("line power budget is invalid")
	}
	return nil
}

// DeviceDraw is one device's contribution to the accounted line load.
type DeviceDraw struct {
	DeviceID string           `json:"device_id"`
	Kind     model.DeviceKind `json:"kind"`
	DrawKW   float64          `json:"draw_kw"`
	Measured bool             `json:"measured"`
	Quality  MeterQuality     `json:"quality"`
}

// HeadroomView is the per-line budget breakdown at one instant.
type HeadroomView struct {
	LineID      model.LineID `json:"line_id"`
	LimitKW     float64      `json:"limit_kw"`
	ReserveKW   float64      `json:"reserve_kw"`
	DrawKW      float64      `json:"draw_kw"`
	AvailableKW float64      `json:"available_kw"`
	Degraded    bool         `json:"degraded"`
	Devices     []DeviceDraw `json:"devices"`
}

// headroom computes the accounted draw per device. A device with a fresh
// meter contributes its measured draw; anything else (stale, rolled over,
// never seen) contributes its nameplate maximum so the reported headroom is
// never optimistic.
func headroom(lineID model.LineID, budget LineBudget, devices []DeviceProfile, meters *MeterBook, now time.Time) HeadroomView {
	view := HeadroomView{
		LineID: lineID, LimitKW: budget.LimitKW, ReserveKW: budget.ReserveKW,
		Devices: make([]DeviceDraw, 0, len(devices)),
	}
	for _, device := range devices {
		draw := DeviceDraw{DeviceID: device.DeviceID, Kind: device.Kind, Quality: meters.Quality(device.DeviceID, now)}
		if measured, ok := meters.MeasuredPower(device.DeviceID, now); ok {
			draw.DrawKW = measured
			draw.Measured = true
		} else {
			draw.DrawKW = device.NameplateKW
			view.Degraded = true
		}
		view.DrawKW += draw.DrawKW
		view.Devices = append(view.Devices, draw)
	}
	sort.Slice(view.Devices, func(i, j int) bool { return view.Devices[i].DeviceID < view.Devices[j].DeviceID })
	view.AvailableKW = view.LimitKW - view.ReserveKW - view.DrawKW
	return view
}

func deviceDrawOf(view HeadroomView, deviceID string) (float64, error) {
	for _, device := range view.Devices {
		if device.DeviceID == deviceID {
			return device.DrawKW, nil
		}
	}
	return 0, fmt.Errorf("device %s is not in the headroom view", deviceID)
}
