package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/config"
)

var (
	discordWebhookRE = regexp.MustCompile(`^https://(?:(?:ptb|canary)\.)?discord(?:app)?\.com/api/(?:v[0-9]{1,2}/)?webhooks/[0-9]{5,25}/[A-Za-z0-9_-]{20,120}$`)
	genericURLRE     = regexp.MustCompile(`^https?://[^\s/]+(?:/\S*)?$`)
)

// CheckWebhookURL valide l'URL saisie. Les messages d'erreur ne répètent jamais l'URL (c'est un secret).
func CheckWebhookURL(url, format string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" || utf8.RuneCountInString(url) > 600 {
		return "", &config.Error{Msg: "URL de notification vide ou trop longue"}
	}
	if format == FormatDiscord {
		if !discordWebhookRE.MatchString(url) {
			return "", &config.Error{Msg: "ce n'est pas une URL de webhook Discord (attendu : https://discord.com/api/webhooks/<id>/<jeton>)"}
		}
	} else if !genericURLRE.MatchString(url) {
		return "", &config.Error{Msg: "URL de notification invalide (attendu : http(s)://hôte/chemin)"}
	}
	return url, nil
}

// WriteSecretFile écrit un fichier secret de façon atomique : droits 600 dès sa création, jamais lisible par d'autres.
func WriteSecretFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".secret-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// SetWebhook enregistre l'URL du webhook dans path (droits 600). L'URL est lue par read (saisie masquée ou entrée
// standard) ; elle n'est JAMAIS un argument de ligne de commande (historique du shell, `ps`).
func SetWebhook(path, format string, dryRun bool, read func() (string, error), log func(string)) error {
	if path == "" {
		return &config.Error{Msg: "notify_url_file n'est pas défini dans [settings] : on ne sait pas où enregistrer l'URL"}
	}
	raw, err := read()
	if err != nil {
		return &config.Error{Msg: "URL de notification vide ou trop longue"}
	}
	url, err := CheckWebhookURL(raw, format)
	if err != nil {
		return err
	}
	if dryRun {
		log(fmt.Sprintf("[simulation] URL valide ; elle serait enregistrée dans %s (droits 600)", path))
		return nil
	}
	if err := WriteSecretFile(path, url+"\n"); err != nil {
		return err
	}
	log(fmt.Sprintf("URL enregistrée dans %s (droits 600, jamais affichée). Vérifiez avec : docker-update notify-test", path))
	return nil
}
