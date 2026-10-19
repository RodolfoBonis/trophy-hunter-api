package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/backlog"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Backlog: GET /catalog/search, GET/POST /wishlist, PATCH/DELETE /wishlist/{id}.

type catalogItemDTO struct {
	Platform     string `json:"platform"`
	StoreID      string `json:"store_id"`
	Title        string `json:"title"`
	Cover        string `json:"cover"`
	PriceCents   *int   `json:"price_cents"`
	RegularCents *int   `json:"regular_cents"`
	Cut          int    `json:"cut"`
	Owned        bool   `json:"owned"`
}

type catalogDTO struct {
	Items []catalogItemDTO `json:"items"`
	// Unavailable: lojas que falharam nesta busca (a UI avisa; as outras seguem).
	Unavailable []string `json:"unavailable"`
}

func (h *Handlers) backlogEnabled(w http.ResponseWriter, r *http.Request) bool {
	if h.backlog == nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotImplemented, "Not implemented", "backlog desligado neste ambiente"))
		return false
	}
	return true
}

func (h *Handlers) searchCatalog(w http.ResponseWriter, r *http.Request) {
	if !h.backlogEnabled(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) < 2 || len(q) > 100 {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "busca com 2 a 100 caracteres"))
		return
	}
	items, unavailable, err := h.backlog.Search(r.Context(), currentUser(r).ID, q)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := catalogDTO{Items: make([]catalogItemDTO, 0, len(items)), Unavailable: nonNilStrings(unavailable)}
	for _, it := range items {
		out.Items = append(out.Items, catalogItemDTO{
			Platform: it.Platform, StoreID: it.StoreID, Title: it.Title, Cover: it.Cover,
			PriceCents: it.PriceCents, RegularCents: it.RegularCents, Cut: it.Cut, Owned: it.Owned,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type wishlistItemDTO struct {
	ID               string     `json:"id"`
	Platform         string     `json:"platform"`
	StoreID          string     `json:"store_id"`
	Title            string     `json:"title"`
	Cover            string     `json:"cover"`
	TargetPriceCents *int32     `json:"target_price_cents"`
	AddedAt          *time.Time `json:"added_at"`
	// Preço: status none (ainda não buscado) | ok | not_found | error.
	PriceStatus    string     `json:"price_status"`
	PriceCents     *int32     `json:"price_cents"`
	RegularCents   *int32     `json:"regular_cents"`
	Cut            int32      `json:"cut"`
	LowCents       *int32     `json:"low_cents"`
	Shop           string     `json:"shop"`
	URL            string     `json:"url"`
	PriceFetchedAt *time.Time `json:"price_fetched_at"`
	// Horas (HowLongToBeat): para platinar/100% e só a história.
	CompletionistH *float32 `json:"completionist_h"`
	MainH          *float32 `json:"main_h"`
}

func toWishlistDTO(x dbgen.ListWishlistRow) wishlistItemDTO {
	d := wishlistItemDTO{
		ID: uuidStr(x.ID), Platform: x.Platform, StoreID: x.StoreID, Title: x.Title, Cover: x.Cover,
		TargetPriceCents: x.TargetPriceCents, AddedAt: tptr(x.AddedAt), PriceStatus: "none",
		PriceCents: x.PriceCents, RegularCents: x.RegularCents, LowCents: x.LowCents,
		PriceFetchedAt: tptr(x.PriceFetchedAt), CompletionistH: x.CompletionistH, MainH: x.MainH,
	}
	if x.PriceStatus != nil {
		d.PriceStatus = *x.PriceStatus
	}
	if x.Cut != nil {
		d.Cut = *x.Cut
	}
	if x.Shop != nil {
		d.Shop = *x.Shop
	}
	if x.PriceUrl != nil {
		d.URL = *x.PriceUrl
	}
	return d
}

func (h *Handlers) listWishlist(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListWishlist(r.Context(), currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]wishlistItemDTO, 0, len(rows))
	for _, x := range rows {
		out = append(out, toWishlistDTO(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

type addWishlistRequest struct {
	Platform         string `json:"platform"`
	StoreID          string `json:"store_id"`
	Title            string `json:"title"`
	Cover            string `json:"cover"`
	TargetPriceCents *int32 `json:"target_price_cents"`
}

func (h *Handlers) addWishlist(w http.ResponseWriter, r *http.Request) {
	if !h.backlogEnabled(w, r) {
		return
	}
	var req addWishlistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.TargetPriceCents != nil && *req.TargetPriceCents < 0) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo inválido"))
		return
	}
	id, err := h.backlog.Add(r.Context(), currentUser(r).ID, req.Platform, req.StoreID, req.Title, req.Cover, req.TargetPriceCents)
	if errors.Is(err, backlog.ErrInvalid) {
		httperr.Write(w, r, httperr.New(httperr.KindValidation, "Validation", "informe platform (steam|psn), store_id e title"))
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": uuidStr(id)})
}

type patchWishlistRequest struct {
	// TargetPriceCents: preço-alvo do aviso; null tira o alvo.
	TargetPriceCents *int32 `json:"target_price_cents"`
}

func (h *Handlers) patchWishlist(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	var req patchWishlistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.TargetPriceCents != nil && *req.TargetPriceCents < 0) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "target_price_cents inválido"))
		return
	}
	n, err := h.q.SetWishlistTarget(r.Context(), dbgen.SetWishlistTargetParams{ID: id, UserID: currentUser(r).ID, Target: req.TargetPriceCents})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "item não encontrado"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) deleteWishlist(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	n, err := h.q.DeleteWishlist(r.Context(), dbgen.DeleteWishlistParams{ID: id, UserID: currentUser(r).ID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "item não encontrado"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
