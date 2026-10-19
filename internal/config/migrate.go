package config

import (
	"fmt"
	"strings"

	"github.com/Bloopps/docker-update/internal/tomlmini"
)

// CurrentVersion est la version du format de configuration que cette version de l'outil écrit (clé [settings] config_version) :
// le nombre de migrations. Un fichier sans cette clé est de version 0. Chaque changement de format qui n'est pas lisible tel
// quel (clé renommée, valeur transformée…) ajoute une Migration à la fin de la table ; l'ajout d'une clé optionnelle n'en demande pas.
func CurrentVersion() int { return len(migrations) }

// Migration transforme le texte d'une configuration de la version From vers From+1 (le texte sort avec ses commentaires).
type Migration struct {
	From  int
	What  string
	Apply func(src string) (string, error)
}

// migrations : dans l'ordre, une par version. 0 -> 1 : la clé config_version apparaît (aucun autre changement).
var migrations = []Migration{
	{From: 0, What: "numéro de version du format (config_version)", Apply: func(s string) (string, error) { return s, nil }},
}

// Version lit config_version ([settings]) ; 0 s'il est absent.
func Version(src []byte) (int, error) {
	doc, err := tomlmini.Parse(src)
	if err != nil {
		return 0, errf("configuration illisible : %v", err)
	}
	t := doc.Tables["settings"]
	if t == nil {
		return 0, nil
	}
	v, ok := t.Get("config_version")
	if !ok {
		return 0, nil
	}
	if !isInt(v) || v.Int < 0 {
		return 0, errf("[settings] config_version : un entier positif ou nul est attendu")
	}
	return int(v.Int), nil
}

// Migrate met le texte d'une configuration au format courant. Il renvoie le nouveau texte (inchangé si déjà à jour) et la
// liste des étapes appliquées. Une configuration écrite par une version plus récente de l'outil est refusée.
func Migrate(src []byte) ([]byte, []string, error) {
	v, err := Version(src)
	if err != nil {
		return nil, nil, err
	}
	if v > CurrentVersion() {
		return nil, nil, errf("la configuration est au format %d, plus récent que celui de cette version de l'outil (%d) : lancer `docker-update update`", v, CurrentVersion())
	}
	if v == CurrentVersion() {
		return src, nil, nil
	}
	text := string(src)
	var steps []string
	for ; v < CurrentVersion(); v++ {
		m := migrations[v]
		if m.From != v {
			return nil, nil, errf("table de migrations incohérente (étape %d)", v)
		}
		if text, err = m.Apply(text); err != nil {
			return nil, nil, errf("migration de la configuration %d -> %d (%s) impossible : %v", v, v+1, m.What, err)
		}
		steps = append(steps, fmt.Sprintf("%d -> %d : %s", v, v+1, m.What))
	}
	return []byte(stampVersion(text, CurrentVersion())), steps, nil
}

// stampVersion écrit config_version = n dans [settings] (remplace la ligne existante, sinon l'insère sous l'en-tête ;
// ajoute la table au début du fichier si elle n'existe pas).
func stampVersion(text string, n int) string {
	line := fmt.Sprintf("config_version = %d", n)
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != "[settings]" {
			continue
		}
		for j := i + 1; j < len(lines); j++ {
			t := strings.TrimSpace(lines[j])
			if strings.HasPrefix(t, "[") {
				break
			}
			if k, _, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "config_version" {
				lines[j] = line
				return strings.Join(lines, "\n")
			}
		}
		lines = append(lines[:i+1], append([]string{line}, lines[i+1:]...)...)
		return strings.Join(lines, "\n")
	}
	return "[settings]\n" + line + "\n\n" + text
}
