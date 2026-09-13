package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type interruptRequest struct {
	SessionID string `json:"sessionId"`
	Profile   string `json:"profile"`
}

func (s *server) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	var req interruptRequest
	if json.NewDecoder(r.Body).Decode(&req) != nil {
		httpError(w, http.StatusBadRequest, "invalid body")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Profile = strings.TrimSpace(req.Profile)
	if req.SessionID == "" {
		httpError(w, http.StatusBadRequest, "sessionId required")
		return
	}
	ctx, cancel := cliCtx(withProfile(r.Context(), req.Profile), 15*time.Second)
	defer cancel()
	se, err := s.findSession(ctx, req.SessionID)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	pane, paneErr := s.sessionPane(ctx, se.ID)
	s.hub.publish(map[string]any{"type": "ask-state", "state": "stopping", "sessionId": se.ID, "ts": time.Now().Unix()})
	stopped, err := s.adapterFor(se).Interrupt(ctx, se, pane, paneErr == nil)
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessionId": se.ID, "stopped": stopped, "state": "stopping"})
}
