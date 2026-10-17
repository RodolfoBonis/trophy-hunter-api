// Package auth é a fronteira de autenticação da API. O app é single-user; o SSO vem do Keycloak
// (realm platform).
//
//   - oidc: valida o JWT (Bearer) contra o JWKS do issuer, conferindo iss, aud (Audience) e a
//     presença da role exigida em realm_access.roles.
//   - dev: confia no header X-Debug-User (apenas dev local, sem Keycloak). Nunca loga tokens.
package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Authenticator valida os tokens conforme o modo configurado. É seguro para uso concorrente
// (o verifier é imutável após a construção).
type Authenticator struct {
	mode         config.AuthMode
	verifier     *oidc.IDTokenVerifier // nil no modo dev
	requiredRole string
}

// New constrói o autenticador. No modo oidc, descobre o provider do issuer e monta o verifier
// com a audience esperada.
func New(ctx context.Context, cfg config.Auth) (*Authenticator, error) {
	if cfg.Mode == config.AuthModeDev {
		return &Authenticator{mode: cfg.Mode, requiredRole: cfg.RequiredRole}, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	return &Authenticator{
		mode: cfg.Mode,
		verifier: provider.Verifier(&oidc.Config{
			ClientID:          cfg.Audience,
			SkipClientIDCheck: cfg.Audience == "",
		}),
		requiredRole: cfg.RequiredRole,
	}, nil
}

// userClaims captura os claims emitidos pelo Keycloak.
type userClaims struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (c userClaims) displayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.PreferredUsername
}

func (c userClaims) hasRole(want string) bool {
	if want == "" {
		return true
	}
	for _, role := range c.RealmAccess.Roles {
		if role == want {
			return true
		}
	}
	return false
}

// Middleware exige um usuário autenticado (e com a role exigida) e injeta o UserContext.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, err := a.authenticate(r)
		if err != nil {
			httperr.Write(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(ContextWithUser(r.Context(), uc)))
	})
}

func (a *Authenticator) authenticate(r *http.Request) (UserContext, error) {
	if a.mode == config.AuthModeDev {
		sub := r.Header.Get("X-Debug-User")
		if sub == "" {
			return UserContext{}, httperr.New(httperr.KindUnauthorized, "Unauthorized",
				"defina X-Debug-User no modo dev")
		}
		return UserContext{Subject: sub, Email: r.Header.Get("X-Debug-Email"), Name: r.Header.Get("X-Debug-Name")}, nil
	}

	raw, ok := bearerToken(r)
	if !ok {
		return UserContext{}, httperr.New(httperr.KindUnauthorized, "Unauthorized", "token Bearer ausente")
	}
	idToken, err := a.verifier.Verify(r.Context(), raw)
	if err != nil {
		return UserContext{}, httperr.New(httperr.KindUnauthorized, "Invalid token",
			"token inválido ou de emissor não confiável")
	}
	var claims userClaims
	if err := idToken.Claims(&claims); err != nil {
		return UserContext{}, httperr.Wrap(httperr.KindInternal, "Internal error", err)
	}
	if !claims.hasRole(a.requiredRole) {
		return UserContext{}, httperr.New(httperr.KindForbidden, "Forbidden",
			"o token não possui a role exigida")
	}
	return UserContext{Subject: idToken.Subject, Email: claims.Email, Name: claims.displayName()}, nil
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}
