package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type seenRequest struct {
	path    string
	headers http.Header
}

func server(t *testing.T) (*httptest.Server, func() []seenRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []seenRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenRequest{r.URL.Path, r.Header.Clone()})
		mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/ok/"):
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.Header().Set("ETag", `"v1"`)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			json.NewEncoder(w).Encode([]map[string]string{{"ghsa_id": "GHSA-2222-3333-4444"}})
		case strings.HasPrefix(r.URL.Path, "/repos/broken/"):
			w.Write([]byte("{pas du json"))
		case strings.HasPrefix(r.URL.Path, "/repos/trailing/"):
			w.Write([]byte("[] []"))
		case strings.HasPrefix(r.URL.Path, "/repos/empty/"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/repos/limit/"):
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("{}"))
		case strings.HasPrefix(r.URL.Path, "/repos/big/"):
			w.Write([]byte(`[{"tag_name": "v1.0.0", "body": "` + strings.Repeat("x", 3<<20) + `"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("{}"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []seenRequest { mu.Lock(); defer mu.Unlock(); return append([]seenRequest(nil), seen...) }
}

func TestGetOKHeadersAndEtag(t *testing.T) {
	srv, seen := server(t)
	code, body, etag := Get(srv.URL+"/repos/ok/x/security-advisories", "", "")
	if code != 200 || etag != `"v1"` {
		t.Fatalf("%d %q", code, etag)
	}
	list, _ := body.([]any)
	if len(list) != 1 || list[0].(map[string]any)["ghsa_id"] != "GHSA-2222-3333-4444" {
		t.Errorf("corps : %v", body)
	}
	h := seen()[0].headers
	if !strings.HasPrefix(h.Get("User-Agent"), "docker-update") || !strings.Contains(h.Get("Accept"), "github") ||
		h.Get("X-GitHub-Api-Version") != "2022-11-28" {
		t.Errorf("en-têtes : %v", h)
	}
	if _, has := h["Authorization"]; has {
		t.Error("pas de jeton configuré : aucun en-tête d'autorisation")
	}
	if _, has := h["If-None-Match"]; has {
		t.Error("pas d'ETag connu : pas de requête conditionnelle")
	}
}

func TestGetTokenAndConditionalRequest(t *testing.T) {
	srv, seen := server(t)
	code, body, etag := Get(srv.URL+"/repos/ok/x/security-advisories", "tok123", `"v1"`)
	if code != 304 || body != nil || etag != `"v1"` {
		t.Errorf("%d %v %q", code, body, etag)
	}
	h := seen()[0].headers
	if h.Get("Authorization") != "Bearer tok123" || h.Get("If-None-Match") != `"v1"` {
		t.Errorf("en-têtes : %v", h)
	}
}

func TestGetErrors(t *testing.T) {
	srv, _ := server(t)
	for path, want := range map[string]int{"/repos/missing/x/security-advisories": 404, "/repos/limit/x/security-advisories": 403,
		"/repos/broken/x/security-advisories": 0, "/repos/trailing/x/security-advisories": 0, "/repos/empty/x/security-advisories": 0} {
		code, body, _ := Get(srv.URL+path, "", "")
		if code != want || (want != 0 && body != nil) {
			t.Errorf("%s : %d %v ; attendu %d", path, code, body, want)
		}
	}
	srv.Close()
	if code, _, _ := Get(srv.URL+"/repos/ok/x/security-advisories", "", ""); code != 0 {
		t.Errorf("connexion refusée : %d", code)
	}
	if code, _, _ := Get("://pas une url", "", ""); code != 0 {
		t.Errorf("URL invalide : %d", code)
	}
}

// Les notes de projets comme Node.js dépassent 2 Mio : la liste des releases a une limite de lecture plus large.
func TestReleasesEndpointAcceptsLargeBodies(t *testing.T) {
	srv, _ := server(t)
	code, body, _ := Get(srv.URL+"/repos/big/x/releases?per_page=30", "", "")
	if code != 200 {
		t.Fatalf("releases : %d", code)
	}
	if list, _ := body.([]any); len(list) != 1 || list[0].(map[string]any)["tag_name"] != "v1.0.0" {
		t.Errorf("corps : %.80v", body)
	}
	if code, _, _ := Get(srv.URL+"/repos/big/x/security-advisories?per_page=50", "", ""); code != 0 {
		t.Errorf("la limite des avis reste de 2 Mio : %d", code)
	}
	// Dépassement de la limite des releases (8 Mio) : un échec, jamais un JSON tronqué.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"body": "` + strings.Repeat("x", 9<<20) + `"}]`))
	}))
	defer huge.Close()
	if code, _, _ := Get(huge.URL+"/repos/a/b/releases?per_page=30", "", ""); code != 0 {
		t.Errorf("au-delà de 8 Mio : %d", code)
	}
}
