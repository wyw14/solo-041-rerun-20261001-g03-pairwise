package power

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

// MeterQuality describes how much the latest meter reading can be trusted.
type MeterQuality string

const (
	QualityOK      MeterQuality = "ok"
	QualityStale   MeterQuality = "stale"
	QualityWrapped MeterQuality = "wrapped"
	QualityMissing MeterQuality = "missing"
)

// Reading is one telemetry frame from a device power meter. EnergyKWh is the
// cumulative register (may roll over), PowerKW is the instantaneous draw.
type Reading struct {
	DeviceID   string       `json:"device_id"`
	LineID     model.LineID `json:"line_id"`
	Sequence   uint64       `json:"sequence"`
	EnergyKWh  float64      `json:"energy_kwh"`
	PowerKW    float64      `json:"power_kw"`
	ObservedAt time.Time    `json:"observed_at"`
}

// Meter is the tracked state of one device meter.
type Meter struct {
	DeviceID string       `json:"device_id"`
	LineID   model.LineID `json:"line_id"`
	Last     Reading      `json:"last"`
	HasLast  bool         `json:"has_last"`
	Wrapped  bool         `json:"wrapped"`
	Wraps    int          `json:"wraps"`
}

// MeterView is the API-facing snapshot of a meter at a given instant.
type MeterView struct {
	DeviceID   string       `json:"device_id"`
	LineID     model.LineID `json:"line_id"`
	Quality    MeterQuality `json:"quality"`
	PowerKW    float64      `json:"power_kw"`
	EnergyKWh  float64      `json:"energy_kwh"`
	Sequence   uint64       `json:"sequence"`
	Wraps      int          `json:"wraps"`
	ObservedAt time.Time    `json:"observed_at,omitempty"`
}

// MeterBook keeps the newest accepted reading per device and flags readings
// that went stale or rolled over, so headroom math never trusts them blindly.
type MeterBook struct {
	mu            sync.RWMutex
	staleAfter    time.Duration
	wrapTolerance float64
	devices       map[string]model.LineID
	meters        map[string]Meter
}

func newMeterBook(devices []DeviceProfile, staleAfter time.Duration, wrapTolerance float64) (*MeterBook, error) {
	if staleAfter <= 0 || wrapTolerance < 0 {
		return nil, errors.New("meter stale window and wrap tolerance are invalid")
	}
	book := &MeterBook{
		staleAfter:    staleAfter,
		wrapTolerance: wrapTolerance,
		devices:       make(map[string]model.LineID, len(devices)),
		meters:        make(map[string]Meter, len(devices)),
	}
	for _, device := range devices {
		book.devices[device.DeviceID] = device.LineID
		book.meters[device.DeviceID] = Meter{DeviceID: device.DeviceID, LineID: device.LineID}
	}
	return book, nil
}

// Ingest validates and stores a reading. Duplicate or regressed sequences are
// ignored (telemetry retries are expected); anything else invalid is an error.
func (b *MeterBook) Ingest(reading Reading, now time.Time) (Meter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	lineID, known := b.devices[reading.DeviceID]
	if !known {
		return Meter{}, fmt.Errorf("power meter %s is not registered", reading.DeviceID)
	}
	if reading.LineID != "" && reading.LineID != lineID {
		return Meter{}, fmt.Errorf("power meter %s does not belong to line %s", reading.DeviceID, reading.LineID)
	}
	if reading.Sequence == 0 {
		return Meter{}, errors.New("power reading sequence is required")
	}
	if !finite(reading.EnergyKWh) || !finite(reading.PowerKW) || reading.EnergyKWh < 0 || reading.PowerKW < 0 {
		return Meter{}, errors.New("power reading values are invalid")
	}
	meter := b.meters[reading.DeviceID]
	if meter.HasLast && reading.Sequence <= meter.Last.Sequence {
		return meter, nil
	}
	reading.LineID = lineID
	if reading.ObservedAt.IsZero() {
		reading.ObservedAt = now.UTC()
	}
	if meter.HasLast && reading.EnergyKWh < meter.Last.EnergyKWh-b.wrapTolerance {
		// Register rolled backwards: distrust this meter until the next
		// reading confirms the new baseline.
		meter.Wrapped = true
		meter.Wraps++
	} else if meter.Wrapped && reading.EnergyKWh >= meter.Last.EnergyKWh-b.wrapTolerance {
		meter.Wrapped = false
	}
	meter.Last = reading
	meter.HasLast = true
	b.meters[reading.DeviceID] = meter
	return meter, nil
}

// Quality reports the trust level of a device's newest reading at `now`.
func (b *MeterBook) Quality(deviceID string, now time.Time) MeterQuality {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.qualityOf(deviceID, now)
}

func (b *MeterBook) qualityOf(deviceID string, now time.Time) MeterQuality {
	meter, ok := b.meters[deviceID]
	if !ok || !meter.HasLast {
		return QualityMissing
	}
	if meter.Wrapped {
		return QualityWrapped
	}
	if now.Sub(meter.Last.ObservedAt) > b.staleAfter {
		return QualityStale
	}
	return QualityOK
}

// MeasuredPower returns the instantaneous draw only when the reading is
// currently trustworthy; the second return value reports that trust.
func (b *MeterBook) MeasuredPower(deviceID string, now time.Time) (float64, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.qualityOf(deviceID, now) != QualityOK {
		return 0, false
	}
	return b.meters[deviceID].Last.PowerKW, true
}

// Views snapshots every meter, sorted by device id.
func (b *MeterBook) Views(now time.Time) []MeterView {
	b.mu.RLock()
	defer b.mu.RUnlock()
	views := make([]MeterView, 0, len(b.meters))
	for _, meter := range b.meters {
		view := MeterView{
			DeviceID: meter.DeviceID, LineID: meter.LineID,
			Quality: b.qualityOf(meter.DeviceID, now), Wraps: meter.Wraps,
		}
		if meter.HasLast {
			view.PowerKW = meter.Last.PowerKW
			view.EnergyKWh = meter.Last.EnergyKWh
			view.Sequence = meter.Last.Sequence
			view.ObservedAt = meter.Last.ObservedAt
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].DeviceID < views[j].DeviceID })
	return views
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
