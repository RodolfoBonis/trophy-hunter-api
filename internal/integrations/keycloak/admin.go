// Package keycloak fala com a Admin API do Keycloak usando o service account do app
// (trophy-hunter-api-admin, só manage-users no próprio realm). Hoje: apagar o usuário quando ele
// exclui a própria conta. O secret do client nunca sai em log nem em erro.
package keycloak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// ErrNotConfigured indica que não há client admin configurado (dev/teste) — quem chama decide seguir sem Keycloak.
var ErrNotConfigured = errors.New("keycloak admin não configurado")

// Admin apaga usuários do realm do issuer configurado.
type Admin struct {
	issuer       string // https://host/realms/<realm>
	adminBase    string // https://host/admin/realms/<realm>
	clientID     string
	clientSecret string
	http         *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// New monta o cliente a partir do issuer do realm (o mesmo AUTH_ISSUER que valida os tokens).
// clientID/secret vazios devolvem um Admin que responde ErrNotConfigured.
func New(issuer, clientID, clientSecret string, hc *http.Client) (*Admin, error) {
	issuer = strings.TrimRight(issuer, "/")
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	a := &Admin{issuer: issuer, clientID: clientID, clientSecret: clientSecret, http: hc}
	if clientID == "" || clientSecret == "" {
		return a, nil
	}
	i := strings.Index(issuer, "/realms/")
	if i < 0 {
		return nil, fmt.Errorf("keycloak: issuer sem /realms/: %q", issuer)
	}
	a.adminBase = issuer[:i] + "/admin" + issuer[i:]
	return a, nil
}

// Issuer é o issuer do realm que este cliente administra.
func (a *Admin) Issuer() string { return a.issuer }

// DeleteUser apaga o usuário (id = sub do token). Já apagado (404) conta como sucesso.
func (a *Admin) DeleteUser(ctx context.Context, userID string) error {
	if a.adminBase == "" {
		return ErrNotConfigured
	}
	tok, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.adminBase+"/users/"+url.PathEscape(userID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest("keycloak", "delete_user", "error")
		return fmt.Errorf("keycloak: apagar usuário: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		metrics.ExternalAPIRequest("keycloak", "delete_user", "2xx")
		return nil
	default:
		metrics.ExternalAPIRequest("keycloak", "delete_user", "error")
		if resp.StatusCode == http.StatusUnauthorized {
			a.mu.Lock()
			a.token = "" // força token novo na próxima
			a.mu.Unlock()
		}
		return fmt.Errorf("keycloak: apagar usuário: status %d", resp.StatusCode)
	}
}

// accessToken: client_credentials com cache até 30 s antes de vencer.
func (a *Admin) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Now().Before(a.expiresAt) {
		return a.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(url.QueryEscape(a.clientID), url.QueryEscape(a.clientSecret))
	resp, err := a.http.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest("keycloak", "token", "error")
		return "", fmt.Errorf("keycloak: token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		metrics.ExternalAPIRequest("keycloak", "token", "error")
		return "", fmt.Errorf("keycloak: token: status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.AccessToken == "" {
		metrics.ExternalAPIRequest("keycloak", "token", "error")
		return "", errors.New("keycloak: token: resposta inválida")
	}
	metrics.ExternalAPIRequest("keycloak", "token", "2xx")
	a.token = body.AccessToken
	a.expiresAt = time.Now().Add(time.Duration(body.ExpiresIn)*time.Second - 30*time.Second)
	return a.token, nil
}
