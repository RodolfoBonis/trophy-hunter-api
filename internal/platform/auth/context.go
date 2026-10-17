package auth

import "context"

// UserContext são os dados do usuário autenticado, resolvidos do token do Keycloak (realm platform)
// ou do header X-Debug-User em modo dev. Subject é o `sub` do Keycloak — identidade estável.
//
// O app é single-user: o Subject é usado para logs/auditoria, não para particionar dados.
type UserContext struct {
	Subject string
	Email   string
	Name    string
}

type userCtxKey struct{}

// ContextWithUser injeta um UserContext no context. Usado pelo middleware e por testes de handler.
func ContextWithUser(ctx context.Context, uc UserContext) context.Context {
	return context.WithValue(ctx, userCtxKey{}, uc)
}

// UserFromContext recupera o UserContext injetado pelo middleware de usuário.
func UserFromContext(ctx context.Context) (UserContext, bool) {
	c, ok := ctx.Value(userCtxKey{}).(UserContext)
	return c, ok
}
