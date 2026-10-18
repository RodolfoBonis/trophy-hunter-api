package api

import (
	"net/http"
)

// getMe devolve os dados do usuário autenticado (GET /me).
func (h *Handlers) getMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, toMeDTO(currentUser(r)))
}
