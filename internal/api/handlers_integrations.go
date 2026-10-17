package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// listIntegrations devolve as integrações (sem segredos) — GET /integrations.
func (h *Handlers) listIntegrations(w http.ResponseWriter, r *http.Request) {
	integs, err := h.q.ListIntegrations(r.Context())
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	out := make([]integrationDTO, 0, len(integs))
	for _, i := range integs {
		out = append(out, toIntegrationDTO(i))
	}
	writeJSON(w, http.StatusOK, out)
}

type connectSteamRequest struct {
	APIKey          string `json:"api_key"`
	SteamIDOrVanity string `json:"steam_id_or_vanity"`
}

// connectSteam valida e grava a integração Steam — PUT /integrations/steam.
func (h *Handlers) connectSteam(w http.ResponseWriter, r *http.Request) {
	var req connectSteamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.SteamIDOrVanity = strings.TrimSpace(req.SteamIDOrVanity)

	var fields []httperr.FieldError
	if req.APIKey == "" {
		fields = append(fields, httperr.FieldError{Field: "api_key", Code: "required"})
	}
	if req.SteamIDOrVanity == "" {
		fields = append(fields, httperr.FieldError{Field: "steam_id_or_vanity", Code: "required"})
	}
	if len(fields) > 0 {
		httperr.Write(w, r, httperr.Validation("campos obrigatórios ausentes", fields...))
		return
	}

	integ, err := h.syncer.ConnectSteam(r.Context(), req.APIKey, req.SteamIDOrVanity)
	if err != nil {
		httperr.Write(w, r, mapSteamError(err))
		return
	}
	writeJSON(w, http.StatusOK, toIntegrationDTO(integ))
}

// deleteIntegration remove a integração de uma plataforma — DELETE /integrations/{platform}.
func (h *Handlers) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	platform := chi.URLParam(r, "platform")
	if !validPlatform(platform) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "plataforma inválida"))
		return
	}
	if _, err := h.q.GetIntegration(r.Context(), platform); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "integração não encontrada"))
			return
		}
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	if err := h.q.DeleteIntegration(r.Context(), platform); err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mapSteamError traduz erros do fluxo de conexão Steam em erros HTTP.
func mapSteamError(err error) *httperr.Error {
	switch {
	case errors.Is(err, steam.ErrInvalidKey):
		return httperr.New(httperr.KindBadGateway, "Steam rejeitou a credencial", "API key inválida ou sem permissão")
	case errors.Is(err, steam.ErrVanityNotFound):
		return httperr.Validation("vanity URL não encontrada",
			httperr.FieldError{Field: "steam_id_or_vanity", Code: "not_found"})
	case errors.Is(err, steam.ErrProfilePrivate):
		return httperr.New(httperr.KindBadGateway, "Perfil Steam não é público", "torne o perfil público e tente novamente")
	default:
		return httperr.New(httperr.KindBadGateway, "Falha ao validar com a Steam", "tente novamente em instantes")
	}
}

func validPlatform(p string) bool { return p == "steam" || p == "psn" }
