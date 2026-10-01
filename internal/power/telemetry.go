package power

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

// ReadingSource identifies where an electrical reading came from.
type ReadingSource string

const (
	SourceMeter ReadingSource = "meter" // energy meter (cumulative kWh)
	SourcePower ReadingSource = "power" // live power telemetry (W)
)

// Reading is one telemetry observation for a device.
type Reading struct {
	DeviceID string        `json:"device_id"`
	Source   ReadingSource `json:"source"`
	// PowerW is the instantaneous draw. Required for SourcePower; for a meter
	// reading it may be zero and is derived from the energy delta instead.
	PowerW float64 `json:"power_w"`
	// EnergyKWh is the cumulative meter value, used for rollback detection.
	EnergyKWh float64 `json:"energy_kwh,omitempty"`
	// Sequence is the optional monotonically increasing telemetry counter.
	Sequence   uint64    `json:"sequence,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}

// ReadingProblem explains why a device's readings are not trustworthy.
type ReadingProblem string

const (
	ProblemNone     ReadingProblem = ""
	ProblemStale    ReadingProblem = "stale_reading"
	ProblemRollback ReadingProblem = "meter_rollback"
	ProblemGap      ReadingProblem = "sequence_gap"
	ProblemNoneSeen ReadingProblem = "no_reading"
)

type deviceReading struct {
	powerW     float64
	energyKWh  float64
	hasEnergy  bool
	sequence   uint64
	hasSeq     bool
	observedAt time.Time
	// problem is sticky once a rollback/gap is seen: a device must send a
	// resynchronised reading (explicit reset or a fresh monotonic run) before
	// it can contribute to headroom again. Staleness and "never seen" are
	// evaluated live from observedAt and do not live here.
	problem ReadingProblem
}

// TelemetryBook stores the newest valid reading per device and flags stale or
// rolled-back data so the budget never trusts it.
type TelemetryBook struct {
	mu        sync.RWMutex
	staleness time.Duration
	readings  map[string]deviceReading
}

// NewTelemetryBook builds an empty book. A zero or negative staleness falls
// back to the package default.
func NewTelemetryBook(staleness time.Duration) *TelemetryBook {
	if staleness <= 0 {
		staleness = stalenessDefault
	}
	return &TelemetryBook{staleness: staleness, readings: make(map[string]deviceReading)}
}

// Record ingests a reading. Rolled-back energy meters and backwards sequence
// numbers are rejected as errors and leave the device flagged untrusted;
// stale timestamps are accepted (they simply age) but never look fresh.
func (b *TelemetryBook) Record(reading Reading, now time.Time) error {
	if reading.DeviceID == "" {
		return errors.New("power reading device is required")
	}
	switch reading.Source {
	case SourceMeter, SourcePower:
	default:
		return errors.New("power reading source is unsupported")
	}
	if math.IsNaN(reading.PowerW) || math.IsInf(reading.PowerW, 0) ||
		math.IsNaN(reading.EnergyKWh) || math.IsInf(reading.EnergyKWh, 0) {
		return errors.New("power reading contains a non-finite value")
	}
	if reading.PowerW < 0 || reading.EnergyKWh < 0 {
		return errors.New("power reading contains a negative value")
	}
	observed := reading.ObservedAt.UTC()
	if observed.IsZero() {
		observed = now.UTC()
	}
	if observed.After(now.Add(time.Second)) {
		return errors.New("power reading is from the future")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	current, exists := b.readings[reading.DeviceID]

	// A cumulative meter that jumps backwards means the counter was reset,
	// rewound, or a foreign feed was spliced in. Treat it as untrusted.
	if reading.Source == SourceMeter && exists && current.hasEnergy && reading.EnergyKWh < current.energyKWh {
		current.problem = ProblemRollback
		current.observedAt = observed
		b.readings[reading.DeviceID] = current
		return errors.New("energy meter reading rolled back")
	}
	// A sequence that moves backwards indicates a reordered/replayed feed.
	if reading.Sequence != 0 && exists && current.hasSeq && reading.Sequence < current.sequence {
		current.problem = ProblemRollback
		current.observedAt = observed
		b.readings[reading.DeviceID] = current
		return errors.New("telemetry sequence moved backwards")
	}

	powerW := reading.PowerW
	applyPower := reading.Source == SourcePower || reading.PowerW > 0
	// A meter-only reading derives instantaneous power from the energy delta
	// since the previous meter observation (kWh over elapsed seconds -> W).
	// A zero delta (a stopped device) legitimately derives zero watts.
	if reading.Source == SourceMeter && reading.PowerW <= 0 && exists && current.hasEnergy && !current.observedAt.IsZero() {
		elapsed := observed.Sub(current.observedAt).Seconds()
		if elapsed > 0 {
			powerW = (reading.EnergyKWh - current.energyKWh) * 3_600_000 / elapsed
			applyPower = true
		}
	}

	// A fresh monotonic reading clears a soft problem automatically. A meter
	// rollback or sequence regression is a data-integrity event: it stays
	// untrusted until an operator confirms the meter and calls Resynchronise,
	// so a rewound feed cannot launder itself back into trust with one sample.
	if exists && current.problem != ProblemNone && current.problem != ProblemRollback && observed.After(current.observedAt) {
		current.problem = ProblemNone
	}

	current.observedAt = observed
	if reading.Source == SourceMeter {
		current.hasEnergy = true
		current.energyKWh = reading.EnergyKWh
	}
	// Live power (including a legitimate 0 for a stopped device) and any value
	// derived from a meter delta overwrite the stored draw. The very first
	// meter point, with no prior delta, leaves the previous draw untouched.
	if applyPower {
		current.powerW = powerW
	}
	if reading.Sequence != 0 {
		current.hasSeq = true
		current.sequence = reading.Sequence
	}
	b.readings[reading.DeviceID] = current
	return nil
}

// Resynchronise clears a sticky rollback/gap flag for a device after an
// operator or meter reset, establishing a fresh monotonic baseline.
func (b *TelemetryBook) Resynchronise(deviceID string, now time.Time) error {
	if deviceID == "" {
		return errors.New("power reading device is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.readings[deviceID]
	current.problem = ProblemNone
	current.hasEnergy = false
	current.hasSeq = false
	current.observedAt = now.UTC()
	b.readings[deviceID] = current
	return nil
}

func (r deviceReading) effectiveProblem(now time.Time, staleness time.Duration) ReadingProblem {
	if r.problem != ProblemNone {
		return r.problem
	}
	if r.observedAt.IsZero() {
		return ProblemNoneSeen
	}
	if now.Sub(r.observedAt) > staleness {
		return ProblemStale
	}
	return ProblemNone
}

// Status describes the trustworthiness of one device's readings.
type Status struct {
	DeviceID   string         `json:"device_id"`
	PowerW     float64        `json:"power_w"`
	ObservedAt time.Time      `json:"observed_at"`
	AgeSeconds float64        `json:"age_seconds"`
	Trusted    bool           `json:"trusted"`
	Problem    ReadingProblem `json:"problem,omitempty"`
}

// DeviceStatus reports one device. Unknown devices return a "never seen" state.
func (b *TelemetryBook) DeviceStatus(deviceID string, now time.Time) Status {
	b.mu.RLock()
	reading, exists := b.readings[deviceID]
	b.mu.RUnlock()
	status := Status{DeviceID: deviceID, ObservedAt: reading.observedAt}
	if exists {
		status.PowerW = reading.powerW
		status.AgeSeconds = now.Sub(reading.observedAt).Seconds()
	}
	problem := ProblemNoneSeen
	if exists {
		problem = reading.effectiveProblem(now.UTC(), b.staleness)
	}
	status.Problem = problem
	status.Trusted = problem == ProblemNone
	return status
}

// Snapshot reports reading status for the given devices, sorted by ID.
func (b *TelemetryBook) Snapshot(deviceIDs []string, now time.Time) []Status {
	statuses := make([]Status, 0, len(deviceIDs))
	for _, deviceID := range deviceIDs {
		statuses = append(statuses, b.DeviceStatus(deviceID, now))
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].DeviceID < statuses[j].DeviceID })
	return statuses
}
