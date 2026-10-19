package cli

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// checkRepos vérifie que chaque dépôt cité dans `advisories` et `release_notes_repos` existe (`validate --online`).
// Dépôt absent (404) : erreur. Réseau coupé, limite de débit, accès refusé : avertissement (on ne conclut rien).
// Renvoie le nombre d'erreurs. Le jeton n'est jamais affiché.
func checkRepos(cfg *config.Config, env *Env, log func(string)) int {
	users := map[string][]string{}
	for _, s := range cfg.Services {
		for _, r := range append(append([]string{}, s.Advisories...), s.ReleaseNotesRepos...) {
			if !slices.Contains(users[r], s.Name) {
				users[r] = append(users[r], s.Name)
			}
		}
	}
	repos := make([]string, 0, len(users))
	for r := range users {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	token := ""
	if p := cfg.Settings.GithubTokenFile; p != "" {
		if b, err := os.ReadFile(p); err == nil {
			token = textutil.Strip(string(b))
		}
	}
	problems := 0
	for _, r := range repos {
		who := strings.Join(users[r], ", ")
		code, _, _ := env.GHGet(fmt.Sprintf("%s/repos/%s", cfg.Settings.GithubAPI, r), token, "")
		switch {
		case code >= 200 && code < 300:
			log(fmt.Sprintf("dépôt %s : trouvé (%s)", r, who))
		case code == 404:
			log(fmt.Sprintf("ERREUR : dépôt GitHub introuvable : %s (utilisé par %s) ; faute de frappe, dépôt renommé ou privé sans jeton", r, who))
			problems++
		case code == 0:
			log(fmt.Sprintf("AVERTISSEMENT : dépôt %s non vérifié (GitHub injoignable)", r))
		default:
			log(fmt.Sprintf("AVERTISSEMENT : dépôt %s non vérifié (réponse HTTP %d : limite de débit ou accès refusé ?)", r, code))
		}
	}
	return problems
}
