package main

import (
	"net/http"
	"time"
)

// GET /api/rc/profiles — list available agent-deck profiles plus the daemon's
// default (cfg.profile) and the profile the reverse-proxied agent-deck web is
// bound to (cfg.proxyProfile). The PWA uses this to render the header selector
// and to gate the in-app terminal (which can only reach the proxy's profile).
//
// The per-call CLI surface (/api/rc/sessions, reply, history, …) is profile-
// switchable via ?profile=<name>; the terminal/push are not (see main.go).
func (s *server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := cliCtx(r.Context(), 8*time.Second)
	defer cancel()
	p, err := s.listProfiles(ctx)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, mapProfilesResponse(p, s.cfg.profile, s.cfg.proxyProfile))
}

// mapProfilesResponse maps the stock CLI shape into the PWA's stable contract.
// Pure (no I/O) so it can be table-tested.
func mapProfilesResponse(p profilesResp, current, proxy string) map[string]any {
	profiles := make([]map[string]any, 0, len(p.Profiles))
	for _, e := range p.Profiles {
		profiles = append(profiles, map[string]any{"name": e.Name, "isDefault": e.IsDefault})
	}
	return map[string]any{
		"profiles":     profiles,
		"current":      current,
		"proxyProfile": proxy,
	}
}
