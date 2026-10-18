package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// listIntegrations devolve as integrações (sem segredos) — GET /integrations.
func (h *Handlers) listIntegrations(w http.ResponseWriter, r *http.Request) {
	integs, err := h.q.ListIntegrationsForUser(r.Context(), currentUser(r).ID)
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
	Label           string `json:"label"`
	APIKey          string `json:"api_key"`
	SteamIDOrVanity string `json:"steam_id_or_vanity"`
}

// connectSteam conecta uma NOVA conta Steam — POST /integrations/steam. api_key é opcional quando
// já existe outra conta Steam (a key é reaproveitada: ela lê qualquer perfil público).
func (h *Handlers) connectSteam(w http.ResponseWriter, r *http.Request) {
	var req connectSteamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	req.SteamIDOrVanity = strings.TrimSpace(req.SteamIDOrVanity)
	if req.SteamIDOrVanity == "" {
		httperr.Write(w, r, httperr.Validation("campos obrigatórios ausentes",
			httperr.FieldError{Field: "steam_id_or_vanity", Code: "required"}))
		return
	}
	if len(strings.TrimSpace(req.Label)) > maxLabelLen {
		httperr.Write(w, r, httperr.Validation("apelido longo demais",
			httperr.FieldError{Field: "label", Code: "too_long"}))
		return
	}

	integ, err := h.syncer.ConnectSteam(r.Context(), syncpkg.ConnectSteamParams{
		UserID: currentUser(r).ID, Label: req.Label, APIKey: req.APIKey, IDOrVanity: req.SteamIDOrVanity,
	})
	if err != nil {
		httperr.Write(w, r, mapSteamError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toIntegrationDTO(integ))
}

type updateIntegrationRequest struct {
	Label  *string `json:"label"`
	APIKey *string `json:"api_key"`
}

const maxLabelLen = 40

// updateIntegration renomeia a conta e/ou troca a API key — PATCH /integrations/{id}.
func (h *Handlers) updateIntegration(w http.ResponseWriter, r *http.Request) {
	integ, ok := h.loadIntegrationParam(w, r)
	if !ok {
		return
	}
	var req updateIntegrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	ctx := r.Context()
	if req.Label != nil {
		label := strings.TrimSpace(*req.Label)
		if label == "" || len(label) > maxLabelLen {
			httperr.Write(w, r, httperr.Validation("apelido inválido",
				httperr.FieldError{Field: "label", Code: "invalid"}))
			return
		}
		if err := h.q.UpdateIntegrationLabel(ctx, dbgen.UpdateIntegrationLabelParams{ID: integ.ID, Label: label}); err != nil {
			httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
			return
		}
	}
	if req.APIKey != nil {
		if strings.TrimSpace(*req.APIKey) == "" {
			httperr.Write(w, r, httperr.Validation("api key vazia",
				httperr.FieldError{Field: "api_key", Code: "required"}))
			return
		}
		if err := h.syncer.UpdateSteamKey(ctx, integ.ID, *req.APIKey); err != nil {
			httperr.Write(w, r, mapSteamError(err))
			return
		}
	}
	updated, err := h.q.GetIntegration(ctx, integ.ID)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	writeJSON(w, http.StatusOK, toIntegrationDTO(updated))
}

// deleteIntegration remove a conta e, em cascata, os jogos dela — DELETE /integrations/{id}.
func (h *Handlers) deleteIntegration(w http.ResponseWriter, r *http.Request) {
	integ, ok := h.loadIntegrationParam(w, r)
	if !ok {
		return
	}
	if err := h.q.DeleteIntegration(r.Context(), integ.ID); err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// loadIntegrationParam resolve {id} da rota (400 id inválido, 404 inexistente).
func (h *Handlers) loadIntegrationParam(w http.ResponseWriter, r *http.Request) (dbgen.Integration, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return dbgen.Integration{}, false
	}
	integ, err := h.q.GetIntegrationForUser(r.Context(), dbgen.GetIntegrationForUserParams{ID: id, UserID: currentUser(r).ID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "integração não encontrada"))
			return dbgen.Integration{}, false
		}
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return dbgen.Integration{}, false
	}
	return integ, true
}

func mapSteamError(err error) *httperr.Error {
	switch {
	case errors.Is(err, syncpkg.ErrAccountExists):
		return httperr.New(httperr.KindConflict, "Conta já conectada", "essa conta Steam já está conectada")
	case errors.Is(err, syncpkg.ErrAPIKeyRequired):
		return httperr.Validation("api key obrigatória na primeira conta Steam",
			httperr.FieldError{Field: "api_key", Code: "required"})
	case errors.Is(err, syncpkg.ErrNotConnected):
		return httperr.New(httperr.KindNotFound, "Not found", "integração não encontrada")
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

type connectPSNRequest struct {
	Label string `json:"label"`
	NPSSO string `json:"npsso"`
}

// connectPSN conecta uma conta PSN pelo NPSSO — POST /integrations/psn. O NPSSO só é usado para
// obter os tokens; nunca é gravado nem retornado.
func (h *Handlers) connectPSN(w http.ResponseWriter, r *http.Request) {
	var req connectPSNRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	if strings.TrimSpace(req.NPSSO) == "" {
		httperr.Write(w, r, httperr.Validation("npsso obrigatório", httperr.FieldError{Field: "npsso", Code: "required"}))
		return
	}
	if len(strings.TrimSpace(req.Label)) > maxLabelLen {
		httperr.Write(w, r, httperr.Validation("apelido longo demais", httperr.FieldError{Field: "label", Code: "too_long"}))
		return
	}
	integ, err := h.syncer.ConnectPSN(r.Context(), syncpkg.ConnectPSNParams{UserID: currentUser(r).ID, Label: req.Label, NPSSO: req.NPSSO})
	if err != nil {
		httperr.Write(w, r, mapPSNError(err))
		return
	}
	writeJSON(w, http.StatusCreated, toIntegrationDTO(integ))
}

type reconnectRequest struct {
	NPSSO string `json:"npsso"`
}

// reconnectIntegration troca um NPSSO novo para uma conta PSN expirada — POST /integrations/{id}/reconnect.
func (h *Handlers) reconnectIntegration(w http.ResponseWriter, r *http.Request) {
	integ, ok := h.loadIntegrationParam(w, r)
	if !ok {
		return
	}
	var req reconnectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	if strings.TrimSpace(req.NPSSO) == "" {
		httperr.Write(w, r, httperr.Validation("npsso obrigatório", httperr.FieldError{Field: "npsso", Code: "required"}))
		return
	}
	updated, err := h.syncer.ReconnectPSN(r.Context(), integ.ID, req.NPSSO)
	if err != nil {
		httperr.Write(w, r, mapPSNError(err))
		return
	}
	writeJSON(w, http.StatusOK, toIntegrationDTO(updated))
}

func mapPSNError(err error) *httperr.Error {
	switch {
	case errors.Is(err, syncpkg.ErrAccountExists):
		return httperr.New(httperr.KindConflict, "Conta já conectada", "essa conta PSN já está conectada")
	case errors.Is(err, syncpkg.ErrAccountMismatch):
		return httperr.Validation("o NPSSO é de outra conta PSN", httperr.FieldError{Field: "npsso", Code: "account_mismatch"})
	case errors.Is(err, psn.ErrInvalidNPSSO), errors.Is(err, psn.ErrInvalidGrant):
		return httperr.Validation("NPSSO inválido ou expirado", httperr.FieldError{Field: "npsso", Code: "invalid"})
	case errors.Is(err, syncpkg.ErrNotConnected):
		return httperr.New(httperr.KindNotFound, "Not found", "integração não encontrada")
	default:
		return httperr.New(httperr.KindBadGateway, "Falha ao falar com a PSN", "tente novamente em instantes")
	}
}
