// Package notify envoie les notifications (Discord, ntfy ou JSON) et construit le rapport d'un passage.
//
// L'URL du webhook est un secret : elle est lue dans un fichier, ne figure dans aucun message d'erreur ni aucun
// journal. Piège de Go : les erreurs de net/http (*url.Error) contiennent l'URL ; on ne journalise donc que le
// type de l'erreur.
package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

// DiscordMax est la longueur maximale (en caractères) d'un message Discord.
const DiscordMax = 1900

// Formats de notification.
const (
	FormatDiscord = "discord"
	FormatNtfy    = "ntfy"
	FormatJSON    = "json"
)

// PostFunc envoie un message. Elle peut renvoyer une erreur dont le texte contient l'URL : l'appelant n'en journalise que le type.
type PostFunc func(url string, body []byte, headers map[string]string) error

// Notifier envoie un message court.
type Notifier struct {
	URLFile string
	Format  string
	DryRun  bool
	Log     func(string)
	Post    PostFunc
}

// New crée un Notifier qui envoie réellement en HTTP (délai de 10 s).
func New(urlFile, format string, log func(string), dryRun bool) *Notifier {
	return &Notifier{URLFile: urlFile, Format: format, DryRun: dryRun, Log: log, Post: httpPost}
}

// HTTPStatusError est l'erreur d'un POST refusé par le serveur (équivalent de HTTPError en Python).
type HTTPStatusError struct{ Code int }

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

func httpPost(url string, body []byte, headers map[string]string) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return &HTTPStatusError{resp.StatusCode}
	}
	return nil
}

// ErrorKind donne le seul nom du type d'une erreur (jamais son texte : il peut contenir l'URL du webhook).
func ErrorKind(err error) string {
	var status *HTTPStatusError
	var netErr net.Error
	switch {
	case errors.As(err, &status):
		return "HTTPError"
	case errors.As(err, &netErr):
		if netErr.Timeout() {
			return "TimeoutError"
		}
		return "URLError"
	}
	name := strings.TrimPrefix(reflect.TypeOf(err).String(), "*")
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// URL lit l'URL du webhook dans son fichier ; vide si le fichier est absent ou illisible.
func (n *Notifier) URL() string {
	if n.URLFile == "" {
		return ""
	}
	b, err := os.ReadFile(n.URLFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

var icons = map[string]string{"info": "ℹ️", "ok": "✅", "warn": "⚠️", "error": "🚨"}

type discordBody struct {
	Content         string           `json:"content"`
	AllowedMentions map[string][]any `json:"allowed_mentions"`
}

type jsonBody struct {
	Title string `json:"title"`
	Text  string `json:"text"`
	Level string `json:"level"`
}

func encode(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// asciiHeader remplace chaque caractère non ASCII par « ? » (comme `encode("ascii", "replace")`).
func asciiHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

// Send envoie un message. Sans URL (ou fichier illisible) : rien. En simulation : seulement journalisé.
func (n *Notifier) Send(title, text, level string) {
	if n.DryRun {
		n.Log("[simulation] notification : " + title)
		return
	}
	url := n.URL()
	if url == "" {
		return
	}
	headers := map[string]string{"User-Agent": "docker-update/1.0"}
	var body []byte
	switch n.Format {
	case FormatDiscord:
		content := icons[level] + " **" + title + "**\n" + text
		if utf8.RuneCountInString(content) > DiscordMax {
			content = string([]rune(content)[:DiscordMax])
		}
		body = encode(discordBody{Content: content, AllowedMentions: map[string][]any{"parse": {}}})
		headers["Content-Type"] = "application/json"
	case FormatNtfy:
		body = []byte(text)
		headers["Title"] = asciiHeader(title)
		if level == "error" {
			headers["Priority"] = "high"
		} else {
			headers["Priority"] = "default"
		}
	default:
		body = encode(jsonBody{Title: title, Text: text, Level: level})
		headers["Content-Type"] = "application/json"
	}
	if err := n.Post(url, body, headers); err != nil {
		// jamais l'URL (ni le texte de l'erreur, qui peut la contenir) dans le journal
		n.Log(fmt.Sprintf("notification non envoyée (%s)", ErrorKind(err)))
	}
}
