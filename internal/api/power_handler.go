package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
	"github.com/wyw14/cry-97/internal/power"
)

type powerReadingsRequest struct {
	Readings []powerReadingInput `json:"readings"`
}

type powerReadingInput struct {
	DeviceID   string    `json:"device_id"`
	Sequence   uint64    `json:"sequence"`
	EnergyKWh  float64   `json:"energy_kwh"`
	PowerKW    float64   `json:"power_kw"`
	ObservedAt time.Time `json:"observed_at"`
}

type budgetRequest struct {
	LimitKW   float64 `json:"limit_kw"`
	ReserveKW float64 `json:"reserve_kw"`
}

func (s *Server) powerStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.plant.PowerStatus())
}

func (s *Server) receivePowerReadings(w http.ResponseWriter, r *http.Request) {
	var input powerReadingsRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(input.Readings) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("at least one power reading is required"))
		return
	}
	meters := make([]power.Meter, 0, len(input.Readings))
	for _, item := range input.Readings {
		meter, err := s.plant.ReceivePowerReading(r.Context(), power.Reading{
			DeviceID: item.DeviceID, Sequence: item.Sequence,
			EnergyKWh: item.EnergyKWh, PowerKW: item.PowerKW, ObservedAt: item.ObservedAt,
		})
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		meters = append(meters, meter)
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ingested": len(meters), "meters": meters})
}

func (s *Server) updatePowerBudget(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var input budgetRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	budget := power.LineBudget{LimitKW: input.LimitKW, ReserveKW: input.ReserveKW}
	if err := s.plant.SetLinePowerBudget(lineID, budget); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"line_id": lineID, "budget": budget})
}
