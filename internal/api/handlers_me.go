package api

import (
	"net/http"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/auth"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// getMe devolve os dados do usuário autenticado (GET /me).
func (h *Handlers) getMe(w http.ResponseWriter, r *http.Request) {
	uc, ok := auth.UserFromContext(r.Context())
	if !ok {
		httperr.Write(w, r, httperr.New(httperr.KindUnauthorized, "Unauthorized", ""))
		return
	}
	writeJSON(w, http.StatusOK, userDTO{Subject: uc.Subject, Email: uc.Email, Name: uc.Name})
}
