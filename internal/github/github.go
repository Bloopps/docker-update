// Package github est le client de lecture de l'API GitHub (avis de sécurité, releases) : requêtes GET JSON avec ETag.
// Le jeton éventuel ne sert qu'à relever la limite de requêtes ; il n'est jamais journalisé.
package github

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Limites de lecture : les avis sont petits ; une liste de releases (notes volumineuses) a une limite plus large.
const (
	MaxAPIBody      = 2 << 20
	MaxReleasesBody = 8 << 20
)

// GetFunc est la requête injectée dans le cœur : (code HTTP, contenu JSON décodé, ETag). Code 0 si le réseau échoue
// ou si la réponse est illisible.
type GetFunc func(url, token, etag string) (code int, body any, newEtag string)

var client = &http.Client{Timeout: 15 * time.Second}

// Get fait un GET JSON sur l'API GitHub.
func Get(url, token, etag string) (int, any, string) {
	limit := int64(MaxAPIBody)
	if strings.HasSuffix(strings.SplitN(url, "?", 2)[0], "/releases") {
		limit = MaxReleasesBody
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "docker-update/1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, ""
	}
	defer resp.Body.Close()
	newEtag := resp.Header.Get("ETag")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, nil, newEtag
	}
	// limite+1 octets : un corps trop grand est un échec (jamais un JSON tronqué que l'on tenterait de lire)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return 0, nil, ""
	}
	var body any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		return 0, nil, ""
	}
	if _, err := dec.Token(); err != io.EOF { // du texte après le JSON : invalide, comme json.loads
		return 0, nil, ""
	}
	return resp.StatusCode, body, newEtag
}
