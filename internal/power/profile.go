// Package power enforces an electrical power budget for the blowers and return
// pumps of every process line. It accepts device meter and live-power
// telemetry, derives a trustworthy per-line headroom, and gates non-emergency
// device commands by clamping them to the available power or deferring them
// ("peak shaving") until the budget frees up.
//
// The guard never reports headroom that it cannot trust: stale readings or a
// backwards-jumping energy meter force the affected line into an untrusted
// state where increases are suppressed rather than guessed.
package power

import (
	"errors"
	"sort"
	"time"

	"github.com/wyw14/cry-97/internal/model"
)

// Role distinguishes the protected equipment classes.
type Role string

const (
	RoleBlower     Role = "blower"
	RoleReturnPump Role = "return_pump"
)

// Profile describes the electrical envelope of one physical device.
type Profile struct {
	ID     string           `json:"id"`
	LineID model.LineID     `json:"line_id"`
	Kind   model.DeviceKind `json:"kind"`
	Role   Role             `json:"role"`
	// RatedPowerW is the nameplate draw when the device runs at full setpoint.
	RatedPowerW float64 `json:"rated_power_w"`
	// MinPowerW is the draw at the minimum allowed setpoint; the device must
	// never be forced below this while running.
	MinPowerW float64 `json:"min_power_w"`
	// MinSetpoint is the lowest command setpoint that still satisfies the
	// protected process requirement (minimum aeration / minimum return flow).
	MinSetpoint float64 `json:"min_setpoint"`
	// MaxSetpoint is the operating-range ceiling.
	MaxSetpoint float64 `json:"max_setpoint"`
}

func (p Profile) valid() error {
	if p.ID == "" || p.LineID == "" {
		return errors.New("power profile identity is incomplete")
	}
	switch p.Role {
	case RoleBlower, RoleReturnPump:
	default:
		return errors.New("power profile role is unsupported")
	}
	if p.RatedPowerW <= 0 || p.MinPowerW < 0 || p.MinPowerW > p.RatedPowerW {
		return errors.New("power profile power envelope is invalid")
	}
	if p.MinSetpoint < 0 || p.MaxSetpoint <= 0 || p.MinSetpoint > p.MaxSetpoint {
		return errors.New("power profile setpoint range is invalid")
	}
	return nil
}

// PowerW translates a command setpoint into an estimated electrical draw.
// Setpoint 0 means "off"; otherwise the draw interpolates between the minimum
// and rated power over the allowed setpoint span.
func (p Profile) PowerW(setpoint float64) float64 {
	switch {
	case setpoint <= 0:
		return 0
	case setpoint <= p.MinSetpoint:
		return p.MinPowerW
	case setpoint >= p.MaxSetpoint:
		return p.RatedPowerW
	default:
		span := p.MaxSetpoint - p.MinSetpoint
		if span <= 0 {
			return p.RatedPowerW
		}
		fraction := (setpoint - p.MinSetpoint) / span
		return p.MinPowerW + fraction*(p.RatedPowerW-p.MinPowerW)
	}
}

// Registry holds the electrical profiles of the protected fleet.
type Registry struct {
	profiles map[string]Profile
	byLine   map[model.LineID][]Profile
}

// NewRegistry builds a registry and rejects duplicate or malformed profiles.
func NewRegistry(profiles []Profile) (*Registry, error) {
	if len(profiles) == 0 {
		return nil, errors.New("at least one power profile is required")
	}
	registry := &Registry{
		profiles: make(map[string]Profile, len(profiles)),
		byLine:   make(map[model.LineID][]Profile),
	}
	for _, profile := range profiles {
		if err := profile.valid(); err != nil {
			return nil, err
		}
		if _, exists := registry.profiles[profile.ID]; exists {
			return nil, errors.New("duplicate power profile for " + profile.ID)
		}
		registry.profiles[profile.ID] = profile
		registry.byLine[profile.LineID] = append(registry.byLine[profile.LineID], profile)
	}
	for lineID := range registry.byLine {
		line := registry.byLine[lineID]
		sort.Slice(line, func(i, j int) bool { return line[i].ID < line[j].ID })
		registry.byLine[lineID] = line
	}
	return registry, nil
}

// Device returns the profile for one device.
func (r *Registry) Device(deviceID string) (Profile, bool) {
	profile, ok := r.profiles[deviceID]
	return profile, ok
}

// Line returns the profiles for a process line, sorted by device ID.
func (r *Registry) Line(lineID model.LineID) []Profile {
	return append([]Profile(nil), r.byLine[lineID]...)
}

// Lines returns the configured line IDs in ascending order.
func (r *Registry) Lines() []model.LineID {
	lines := make([]model.LineID, 0, len(r.byLine))
	for lineID := range r.byLine {
		lines = append(lines, lineID)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i] < lines[j] })
	return lines
}

// DefaultProfiles assigns conservative electrical envelopes to the blowers and
// A/B return pumps created by the plant wiring. Values are in watts.
func DefaultProfiles(lineIDs []model.LineID) []Profile {
	profiles := make([]Profile, 0, len(lineIDs)*3)
	for _, lineID := range lineIDs {
		profiles = append(profiles, Profile{
			ID: "blower-basin-" + string(lineID), LineID: lineID,
			Kind: model.DeviceBlower, Role: RoleBlower,
			RatedPowerW: 55000, MinPowerW: 21000, MinSetpoint: 28, MaxSetpoint: 60,
		})
		for _, suffix := range []string{"A", "B"} {
			profiles = append(profiles, Profile{
				ID: "pump-" + string(lineID) + "-" + suffix, LineID: lineID,
				Kind: model.DevicePump, Role: RoleReturnPump,
				// Return pumps are fixed-speed: starting one draws its rated
				// load, which is itself the protected minimum return.
				RatedPowerW: 18500, MinPowerW: 18500, MinSetpoint: 1, MaxSetpoint: 1,
			})
		}
	}
	return profiles
}

// stalenessDefault bounds how old a reading may be before it is distrusted.
const stalenessDefault = 45 * time.Second
