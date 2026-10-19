package cli

import (
	"fmt"

	"github.com/Bloopps/docker-update/internal/config"
)

// migrateCmd réécrit la configuration au format courant (après une mise à jour de l'outil). Aperçu, confirmation, sauvegarde
// et validation comme `edit`. Les commandes de passage lisent déjà un ancien format en mémoire : migrer n'est jamais urgent.
// Les unités systemd installées dont la version est plus ancienne sont aussi proposées (aperçu, confirmation, sauvegarde).
// L'état est migré tout seul, avec copie de l'ancien fichier, à sa prochaine écriture.
func migrateCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	p := newPrompter(env)
	code := 0
	if len(cfg.Migrations) == 0 {
		log(fmt.Sprintf("configuration à jour (format %d)", config.CurrentVersion()))
	} else {
		c, err := withConfigLock(cfg, g, log, func(data []byte) (int, error) {
			out, steps, err := config.Migrate(data)
			if err != nil {
				return 0, err
			}
			for _, s := range steps {
				log("migration " + s)
			}
			return applyEdit(env, g, p, sub.flags["--yes"], data, out, log)
		})
		if err != nil || c != 0 {
			return c, err
		}
	}
	units := outdatedUnits(env, g.config)
	if len(units) == 0 {
		log("unités systemd à jour")
		return code, nil
	}
	return migrateUnits(env, g, p, sub.flags["--yes"], units, log)
}
