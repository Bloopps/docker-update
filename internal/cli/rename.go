package cli

import (
	"fmt"
	"strings"

	"github.com/Bloopps/docker-update/internal/config"
	"github.com/Bloopps/docker-update/internal/state"
	"github.com/Bloopps/docker-update/internal/tomledit"
)

// renameCmd renomme un service (ou, avec --group, un groupe) dans le fichier, puis reporte le nouveau nom dans l'état
// (pauses, erreurs, alertes) pour ne laisser aucune entrée orpheline. Aperçu, confirmation et sauvegarde comme add / delete.
func renameCmd(env *Env, g globals, sub subArgs, cfg *config.Config, log func(string)) (int, error) {
	if len(sub.names) != 2 {
		return 0, usagef("rename : deux noms sont attendus (ancien puis nouveau)")
	}
	old, newName := sub.names[0], sub.names[1]
	group := sub.flags["--group"]
	return withConfigLock(cfg, g, log, func(data []byte) (int, error) {
		f, err := tomledit.Parse(data)
		if err != nil {
			return 0, editErr(err)
		}
		cur, err := checkResult(data)
		if err != nil {
			return 0, &config.Error{Msg: err.Error()}
		}
		fail := func(format string, a ...any) (int, error) {
			log("refusé : " + fmt.Sprintf(format, a...))
			return 1, nil
		}
		if old == newName {
			return fail("l'ancien et le nouveau nom sont identiques")
		}
		if group {
			var members []string
			for _, s := range cur.Services {
				if s.Group == old {
					members = append(members, s.Name)
				}
			}
			if len(members) == 0 {
				return fail("groupe inconnu : %s", old)
			}
			for _, m := range members {
				if ok, err := f.SetString(m, "group", newName); err != nil {
					return 0, editErr(err)
				} else if !ok {
					return fail("la clé group de %s n'est pas écrite dans son propre bloc : modification à la main", m)
				}
			}
		} else {
			if f.Index(old) < 0 {
				return fail("service inconnu : %s (services : %s)", old, strings.Join(f.Names(), ", "))
			}
			if _, err := f.SetString(old, "name", newName); err != nil {
				return 0, editErr(err)
			}
		}
		// Les collisions (nom ou groupe déjà pris) et les noms invalides sont refusés par la validation de applyEdit.
		code, err := applyEdit(env, g, newPrompter(env), sub.flags["--yes"], data, f.Bytes(), log)
		if err != nil || code != 0 || g.dryRun {
			return code, err
		}
		return 0, renameState(cfg, group, old, newName, log)
	})
}

// renameState reporte le nouveau nom dans le fichier d'état : pauses (nom de service ou de groupe), erreurs et alertes
// (le nom du service est le deuxième segment de la clé).
func renameState(cfg *config.Config, group bool, old, newName string, log func(string)) error {
	st, err := state.Load(cfg.Settings.StateFile)
	if err != nil {
		return err
	}
	changed := 0
	for i, h := range st.Holds {
		if h == old {
			st.Holds[i] = newName
			changed++
		}
	}
	if !group {
		if v, ok := st.Errors[old]; ok {
			delete(st.Errors, old)
			st.Errors[newName] = v
			changed++
		}
		for k, v := range st.Alerts {
			parts := strings.SplitN(k, ":", 3)
			if len(parts) >= 2 && parts[1] == old {
				parts[1] = newName
				delete(st.Alerts, k)
				st.Alerts[strings.Join(parts, ":")] = v
				changed++
			}
		}
	}
	if changed == 0 {
		return nil
	}
	if err := st.Save(); err != nil {
		return err
	}
	log(fmt.Sprintf("état : %d entrée(s) reportée(s) sous le nouveau nom (pauses, erreurs, alertes)", changed))
	return nil
}
