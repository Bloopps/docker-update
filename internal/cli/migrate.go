package cli

import (
	"fmt"

	"github.com/Bloopps/docker-update/internal/config"
)

// migrateCmd réécrit la configuration au format courant (après une mise à jour de l'outil). Aperçu, confirmation, sauvegarde
// et validation comme `edit`. Les commandes de passage lisent déjà un ancien format en mémoire : migrer n'est jamais urgent.
// L'état est migré tout seul, avec copie de l'ancien fichier, à sa prochaine écriture.
func migrateCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	if len(cfg.Migrations) == 0 {
		log(fmt.Sprintf("configuration à jour (format %d)", config.CurrentVersion()))
		return 0, nil
	}
	return withConfigLock(cfg, g, log, func(data []byte) (int, error) {
		out, steps, err := config.Migrate(data)
		if err != nil {
			return 0, err
		}
		for _, s := range steps {
			log("migration " + s)
		}
		return applyEdit(env, g, newPrompter(env), sub.flags["--yes"], data, out, log)
	})
}
