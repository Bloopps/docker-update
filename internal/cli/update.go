package cli

import (
	"github.com/Bloopps/docker-update/internal/selfupdate"
)

// updateCmd met le binaire à jour depuis la dernière release. Rien d'autre n'est touché : ni configuration, ni état, ni conteneur.
// --check et --dry-run se contentent de comparer les versions.
func updateCmd(env *Env, g globals, sub subArgs, log func(string)) (int, error) {
	d := env.UpdateDeps()
	_, err := selfupdate.Run(d, sub.flags["--check"] || g.dryRun, log)
	return 0, err
}
