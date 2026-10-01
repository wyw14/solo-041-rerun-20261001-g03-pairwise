package api

import (
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-97/internal/model"
)

func (s *Server) aerationStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"blowers": s.plant.StatusBlowers(), "windows": s.plant.StatusAerationWindows(),
	})
}

func (s *Server) aerationSample(w http.ResponseWriter, r *http.Request) {
	lineID, err := model.ParseLineID(chi.URLParam(r, "lineID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	window, command, suppression, err := s.plant.SubmitAeration(r.Context(), lineID, chi.URLParam(r, "basinID"), payload)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"window": window, "command": command, "suppression": suppression})
}
