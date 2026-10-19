package keycloak

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeleteUser(t *testing.T) {
	var tokens, deletes atomic.Int32
	var deleteStatus atomic.Int32
	deleteStatus.Store(http.StatusNoContent)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/realms/th/protocol/openid-connect/token":
			id, secret, _ := r.BasicAuth()
			if id != "admin" || secret != "s3cr3t" || r.FormValue("grant_type") != "client_credentials" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			tokens.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"tok","expires_in":300}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/admin/realms/th/users/u-1":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			deletes.Add(1)
			w.WriteHeader(int(deleteStatus.Load()))
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	a, err := New(srv.URL+"/realms/th", "admin", "s3cr3t", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.DeleteUser(ctx, "u-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Já apagado: 404 conta como sucesso; o token fica em cache.
	deleteStatus.Store(http.StatusNotFound)
	if err := a.DeleteUser(ctx, "u-1"); err != nil {
		t.Fatalf("delete 404: %v", err)
	}
	if tokens.Load() != 1 || deletes.Load() != 2 {
		t.Fatalf("tokens=%d deletes=%d", tokens.Load(), deletes.Load())
	}
	deleteStatus.Store(http.StatusForbidden)
	if err := a.DeleteUser(ctx, "u-1"); err == nil {
		t.Fatal("403 deveria falhar")
	}
}

func TestNotConfigured(t *testing.T) {
	a, err := New("https://auth.example/realms/th", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteUser(context.Background(), "u"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretNotInErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	a, _ := New(srv.URL+"/realms/th", "admin", "super-secret-value", srv.Client())
	err := a.DeleteUser(context.Background(), "u")
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("err = %v", err)
	}
}
