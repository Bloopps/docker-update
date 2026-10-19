package cli

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// usageError est une erreur de ligne de commande (code de sortie 2, comme argparse).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

// options globales : elles précèdent la sous-commande (comme avec argparse, elles ne sont pas reconnues après).
type globals struct {
	config         string
	dryRun         bool
	allowUntrusted bool
	help           bool
	version        bool
}

const defaultConfig = "/etc/docker-update/updates.toml"

// matchLong cherche une option longue : nom exact, ou préfixe unique (comme argparse).
func matchLong(name string, known []string) (string, error) {
	for _, k := range known {
		if k == name {
			return k, nil
		}
	}
	var found []string
	for _, k := range known {
		if strings.HasPrefix(k, name) {
			found = append(found, k)
		}
	}
	switch len(found) {
	case 0:
		return "", usagef("option inconnue : %s", name)
	case 1:
		return found[0], nil
	}
	sort.Strings(found)
	return "", usagef("option ambiguë : %s (%s)", name, strings.Join(found, ", "))
}

// parseGlobals lit les options avant la sous-commande ; renvoie la sous-commande et ses arguments.
func parseGlobals(args []string) (g globals, cmd string, rest []string, err error) {
	g.config = defaultConfig
	known := []string{"--config", "--dry-run", "--allow-untrusted-config", "--help", "--version"}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			if i < len(args) {
				return g, args[i], args[i+1:], nil
			}
		case a == "-h":
			g.help = true
			i++
		case strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a, "=")
			full, err := matchLong(name, known)
			if err != nil {
				return g, "", nil, err
			}
			i++
			switch full {
			case "--config":
				if !hasValue {
					if i >= len(args) || (strings.HasPrefix(args[i], "-") && args[i] != "-") {
						return g, "", nil, usagef("l'option --config attend un argument")
					}
					value = args[i]
					i++
				}
				g.config = value
			case "--dry-run", "--allow-untrusted-config", "--help", "--version":
				if hasValue {
					return g, "", nil, usagef("l'option %s n'attend pas d'argument", full)
				}
				switch full {
				case "--dry-run":
					g.dryRun = true
				case "--allow-untrusted-config":
					g.allowUntrusted = true
				case "--help":
					g.help = true
				case "--version":
					g.version = true
				}
			}
		case strings.HasPrefix(a, "-") && a != "-":
			return g, "", nil, usagef("option inconnue : %s", a)
		default:
			return g, a, args[i+1:], nil
		}
	}
	return g, "", nil, nil
}

// sousCommande décrit les arguments d'une sous-commande.
type subSpec struct {
	names   string   // "" : aucun nom ; "*" : zéro nom ou plus ; "+" : au moins un ; "1" : exactement un
	flags   []string // options longues connues (sans valeur)
	valued  []string // options longues avec une valeur (--option valeur ou --option=valeur), répétables
	optBool []string // options booléennes qui acceptent aussi =true / =false (--cleanup, --cleanup=false)
}

var subcommands = map[string]subSpec{
	"validate":    {},
	"check":       {names: "*", flags: []string{"--notify"}},
	"run":         {names: "*"},
	"status":      {},
	"notify-test": {},
	"set-webhook": {},
	"hold":        {names: "+"},
	"unhold":      {names: "+"},
	"now":         {names: "+", flags: []string{"--yes"}},
	"version":     {},
	"list":        {},
	"init":        {flags: []string{"--yes", "--check", "--force", "--skip-checks"}, valued: []string{"--mode", "--include", "--exclude"}},
	"delete":      {names: "*", flags: []string{"--keep-state", "--yes"}, valued: []string{"--group"}},
	"add": {names: "1", flags: []string{"--release-notes", "--yes", "--first"}, optBool: []string{"--cleanup"},
		valued: []string{"--compose-dir", "--service", "--compose-file", "--mode", "--cooldown", "--window", "--group",
			"--container", "--advisories", "--min-severity", "--security-cooldown", "--security-policy", "--health-timeout",
			"--release-notes-repos", "--release-notes-ignore", "--smoke-url", "--smoke-expect", "--after"}},
}

var commandOrder = []string{"validate", "check", "run", "now", "hold", "unhold", "status", "notify-test", "set-webhook", "init", "list", "add", "delete", "version"}

type subArgs struct {
	names  []string
	flags  map[string]bool
	values map[string][]string // options avec valeur, dans l'ordre de la ligne de commande
}

// parseSub lit les arguments d'une sous-commande. Les options peuvent être mêlées aux noms ; `--` termine les options.
// Un argument en trop n'est jamais répété dans le message pour `set-webhook` : l'URL du webhook pourrait y figurer.
func parseSub(cmd string, args []string) (subArgs, error) {
	spec := subcommands[cmd]
	out := subArgs{names: []string{}, flags: map[string]bool{}, values: map[string][]string{}}
	known := append(append(append([]string{}, spec.flags...), spec.valued...), spec.optBool...)
	onlyNames := false
	var extra int
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case !onlyNames && a == "--":
			onlyNames = true
		case !onlyNames && strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a, "=")
			full, err := matchLong(name, known)
			if err != nil {
				return out, usagef("%s : option inconnue ou invalide", cmd)
			}
			switch {
			case slices.Contains(spec.valued, full):
				if !hasValue {
					if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
						return out, usagef("%s : l'option %s attend une valeur", cmd, full)
					}
					i++
					value = args[i]
				}
				out.values[full] = append(out.values[full], value)
			case slices.Contains(spec.optBool, full):
				switch {
				case !hasValue || value == "true":
					out.values[full] = append(out.values[full], "true")
				case value == "false":
					out.values[full] = append(out.values[full], "false")
				default:
					return out, usagef("%s : %s accepte seulement true ou false", cmd, full)
				}
			default:
				if hasValue {
					return out, usagef("%s : option inconnue ou invalide", cmd)
				}
				out.flags[full] = true
			}
		case !onlyNames && strings.HasPrefix(a, "-") && a != "-":
			return out, usagef("%s : option inconnue ou invalide", cmd)
		default:
			if spec.names == "" {
				extra++
				continue
			}
			out.names = append(out.names, a)
		}
	}
	if extra > 0 {
		return out, usagef("%s : %d argument(s) inattendu(s) (non répétés ici)", cmd, extra)
	}
	if spec.names == "+" && len(out.names) == 0 {
		return out, usagef("%s : au moins un nom est requis", cmd)
	}
	if spec.names == "1" && len(out.names) != 1 {
		return out, usagef("%s : un seul nom est attendu", cmd)
	}
	return out, nil
}

const helpText = `docker-update : mises à jour d'images pour des piles Docker Compose, avec délai de sécurité.

Utilisation : docker-update [--config FICHIER] [--dry-run] [--allow-untrusted-config] COMMANDE [...]

Les options viennent AVANT la commande.

Commandes :
  validate            vérifie la configuration et les outils, ne touche à rien
  check [NOM...]      scan sans rien appliquer : tous les services, ou ceux désignés (--notify : envoie le tableau d'état)
  run [NOM...]        scan, applique le « auto » prêt, notifie : tous les services, ou ceux désignés
  now NOM... --yes    applique tout de suite (sans délai ni fenêtre) les services ou groupes nommés
  hold NOM...         met en pause ; unhold NOM... reprend
  status              affiche l'état mémorisé
  notify-test         envoie un message de test
  set-webhook         enregistre l'URL du webhook (saisie masquée ou entrée standard, jamais en argument)
  init                vérifie l'installation, puis crée la configuration à partir des conteneurs qui tournent (init --check : vérifications seules)
  list                tableau des services configurés (lecture seule)
  add NOM [options]   ajoute un service au fichier de configuration (aperçu, confirmation, sauvegarde, validation)
  delete NOM...       retire des services du fichier (ou --group G) ; ne touche à aucun conteneur ni image
  version             affiche la version

Options :
  --config FICHIER           configuration (défaut : ` + defaultConfig + `)
  --dry-run                  n'exécute aucune commande qui modifie quelque chose
  --allow-untrusted-config   ne pas exiger une configuration appartenant à root (tests uniquement)
`
