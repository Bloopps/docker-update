package smoke

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStatusDoesNotFollowRedirects(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, target.URL, http.StatusFound)
		case "/ua":
			if r.Header.Get("User-Agent") != "docker-update/1.0" {
				w.WriteHeader(http.StatusTeapot)
			}
		case "/denied":
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	for path, want := range map[string]int{"/redirect": 302, "/ua": 200, "/denied": 403, "/nope-ok": 200} {
		if got := Status(srv.URL+path, time.Second); got != want {
			t.Errorf("%s : %d ; attendu %d", path, got, want)
		}
	}
	if hits != 0 {
		t.Errorf("la redirection a été suivie (%d requête(s))", hits)
	}
}

func TestStatusUnreachable(t *testing.T) {
	if got := Status("http://127.0.0.1:9/", time.Second); got != 0 {
		t.Errorf("injoignable : %d", got)
	}
	if got := Status("://invalide", time.Second); got != 0 {
		t.Errorf("URL invalide : %d", got)
	}
}
