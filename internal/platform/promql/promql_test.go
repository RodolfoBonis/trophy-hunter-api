package promql

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == "ruim" {
			_, _ = w.Write([]byte(`{"status":"error","error":"parse error"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"kind":"render"},"value":[1,"3"]},{"metric":{"kind":"x"},"value":[1,"NaN"]}]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	got, err := c.Query(context.Background(), "sum by (kind) (x)")
	if err != nil || len(got) != 1 || got[0].Labels["kind"] != "render" || got[0].Value != 3 {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := c.Query(context.Background(), "ruim"); err == nil {
		t.Fatal("erro do prometheus deveria voltar")
	}
	if _, err := New("", nil).Query(context.Background(), "up"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("sem url: %v", err)
	}
}
