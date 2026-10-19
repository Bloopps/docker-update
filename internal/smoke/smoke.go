// Package smoke fait le test de fumée HTTP d'un service après sa mise à jour.
package smoke

import (
	"io"
	"net/http"
	"time"
)

// Status renvoie le code HTTP d'un GET sans suivre les redirections ; 0 si la connexion échoue.
func Status(url string, timeout time.Duration) int {
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("User-Agent", "docker-update/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode
}

// Default est la vérification utilisée par défaut : délai de 10 s.
func Default(url string) int { return Status(url, 10*time.Second) }
