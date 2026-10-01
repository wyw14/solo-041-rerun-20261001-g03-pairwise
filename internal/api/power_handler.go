package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/power"
)

// powerStatus assembles the operator budget board.
type powerStatus struct {
	Headroom   []power.LineHeadroom      `json:"headroom"`
	Suppressed []power.SuppressedCommand `json:"suppressed"`
	Decisions  []power.Decision          `json:"decisions"`
	ObservedAt time.Time                 `json:"observed_at"`
}

type powerTelemetryRequest struct {
	DeviceID   string  `json:"device_id"`
	Source     string  `json:"source"`
	PowerW     float64 `json:"power_w"`
	EnergyKWh  float64 `json:"energy_kwh"`
	Sequence   uint64  `json:"sequence"`
	ObservedAt string  `json:"observed_at"`
}

type powerCommandRequest struct {
	DeviceID string  `json:"device_id"`
	Kind     string  `json:"kind"`
	Setpoint float64 `json:"setpoint"`
}

func (s *Server) powerBoard(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, powerStatus{
		Headroom:   s.plant.PowerHeadroom(),
		Suppressed: s.plant.PowerSuppressed(),
		Decisions:  s.plant.PowerHistory(),
		ObservedAt: s.plant.Clock(),
	})
}

func (s *Server) powerTelemetry(w http.ResponseWriter, r *http.Request) {
	var input powerTelemetryRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	source := power.ReadingSource(input.Source)
	if source != power.SourceMeter && source != power.SourcePower {
		writeError(w, http.StatusBadRequest, errUnsupported("power reading source"))
		return
	}
	observedAt := s.plant.Clock()
	if input.ObservedAt != "" {
		parsed, err := time.Parse(time.RFC3339, input.ObservedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		observedAt = parsed
	}
	reading := power.Reading{
		DeviceID: input.DeviceID, Source: source, PowerW: input.PowerW,
		EnergyKWh: input.EnergyKWh, Sequence: input.Sequence, ObservedAt: observedAt,
	}
	released, err := s.plant.ReceivePowerReading(r.Context(), reading)
	if err != nil {
		// A rollback is a recorded data event; report 409 so the operator sees
		// the headroom has gone untrusted rather than silently accepting it.
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"reading": reading, "released": released,
		"headroom": s.plant.PowerHeadroom(),
	})
}

func (s *Server) powerCommand(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input powerCommandRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var kind model.DeviceKind
	switch input.Kind {
	case string(model.DeviceBlower):
		kind = model.DeviceBlower
	case string(model.DevicePump):
		kind = model.DevicePump
	default:
		writeError(w, http.StatusBadRequest, errUnsupported("device kind; expected blower or pump"))
		return
	}
	decision, err := s.plant.ManualDeviceCommand(r.Context(), lineID, input.DeviceID, kind, input.Setpoint)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	// A suppressed command is not a request failure: 202 carries the verdict
	// and recovery conditions for the operator page.
	writeJSON(w, http.StatusAccepted, decision)
}

func (s *Server) powerResync(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if err := s.plant.ResynchronisePower(deviceID); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resynchronised": deviceID})
}

type simpleError string

func (e simpleError) Error() string { return string(e) }

func errUnsupported(message string) error { return simpleError(message + " is unsupported") }
