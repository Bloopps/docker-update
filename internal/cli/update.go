package cli

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/selfupdate"
)

// ghToken cherche un jeton GitHub pour lire une release d'un dépôt privé : GH_TOKEN, GITHUB_TOKEN, puis `gh auth token`
// (si gh est installé et connecté pour cet utilisateur). Vide si rien n'est trouvé : téléchargements publics.
func ghToken(getenv func(string) string) string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
	}
	path, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// updateCmd met le binaire à jour depuis la dernière release. Rien d'autre n'est touché : ni configuration, ni état, ni conteneur.
// --check et --dry-run se contentent de comparer les versions.
func updateCmd(env *Env, g globals, sub subArgs, log func(string)) (int, error) {
	d := env.UpdateDeps()
	_, err := selfupdate.Run(d, sub.flags["--check"] || g.dryRun, log)
	return 0, err
}
