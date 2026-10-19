// Package discover relève, en lecture seule, les conteneurs Docker Compose qui tournent, pour `init` et `add --container`.
//
// Jamais d'environnement ni de sortie de `docker compose config` dans un message ou un journal : seules des champs précis
// (étiquettes, nom du projet) en sont extraits.
package discover

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// Container est un conteneur Compose qui tourne.
type Container struct {
	ID          string
	Name        string // nom du conteneur, sans « / »
	Image       string // référence d'image telle que déclarée (Config.Image)
	Project     string
	Service     string
	WorkingDir  string
	ConfigFiles []string
}

const (
	sep           = "\x1f"
	inspectFormat = "{{.ID}}" + sep + "{{.Name}}" + sep + "{{.Config.Image}}" + sep +
		`{{index .Config.Labels "com.docker.compose.project"}}` + sep +
		`{{index .Config.Labels "com.docker.compose.service"}}` + sep +
		`{{index .Config.Labels "com.docker.compose.project.working_dir"}}` + sep +
		`{{index .Config.Labels "com.docker.compose.project.config_files"}}`
)

// Running liste les conteneurs Compose qui tournent (docker ps -q puis docker inspect --format : jamais l'environnement).
// Les conteneurs hors Compose (sans étiquettes de projet) sont ignorés. Résultat trié par projet puis par service.
func Running(run dockerx.Runner) ([]Container, error) {
	ps := run.Run([]string{"docker", "ps", "-q"}, false, dockerx.DefaultRunTimeout)
	if ps.RC != 0 {
		return nil, fmt.Errorf("docker ps a échoué : %s", textutil.Truncate(textutil.Strip(ps.Err), 200))
	}
	ids := textutil.Fields(ps.Out)
	if len(ids) == 0 {
		return nil, nil
	}
	argv := append([]string{"docker", "inspect", "--format", inspectFormat}, ids...)
	in := run.Run(argv, false, dockerx.DefaultRunTimeout)
	if in.RC != 0 {
		return nil, fmt.Errorf("docker inspect a échoué : %s", textutil.Truncate(textutil.Strip(in.Err), 200))
	}
	var out []Container
	for _, line := range strings.Split(in.Out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), sep)
		if len(f) != 7 || f[3] == "" || f[4] == "" || f[5] == "" {
			continue
		}
		c := Container{ID: f[0], Name: strings.TrimPrefix(f[1], "/"), Image: f[2], Project: f[3], Service: f[4], WorkingDir: f[5]}
		for _, file := range strings.Split(f[6], ",") {
			if file = strings.TrimSpace(file); file != "" {
				c.ConfigFiles = append(c.ConfigFiles, file)
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

// composeBase est le début de toute commande compose de l'outil pour ce conteneur.
func composeBase(c Container) []string {
	argv := []string{"docker", "compose", "--project-directory", c.WorkingDir}
	for _, f := range ComposeFiles(c) {
		argv = append(argv, "-f", f)
	}
	return argv
}

var defaultNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// ComposeFiles renvoie la valeur de compose_files à écrire : vide quand le projet n'a qu'un fichier compose par défaut
// (le dossier suffit), sinon la liste telle que Compose l'a chargée.
func ComposeFiles(c Container) []string {
	if len(c.ConfigFiles) == 1 {
		f := c.ConfigFiles[0]
		for _, n := range defaultNames {
			if f == path.Join(c.WorkingDir, n) {
				return nil
			}
		}
	}
	return c.ConfigFiles
}

// Verify contrôle que l'outil retrouvera bien ce conteneur : `docker compose config` donne le même nom de projet que
// l'étiquette, et `docker compose ps -q --all SERVICE` désigne ce conteneur. Renvoie une raison lisible en cas d'échec ;
// ne contient jamais la sortie de `config`.
func Verify(run dockerx.Runner, c Container) (string, bool) {
	cfg := run.Run(append(composeBase(c), "config", "--format", "json"), false, dockerx.DefaultRunTimeout)
	if cfg.RC != 0 {
		return "docker compose config a échoué : " + textutil.Truncate(textutil.Strip(cfg.Err), 200), false
	}
	var doc struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(cfg.Out), &doc) != nil || doc.Name == "" {
		return "docker compose config n'indique pas de nom de projet", false
	}
	if doc.Name != c.Project {
		return fmt.Sprintf("le projet vu par compose (%s) n'est pas celui du conteneur (%s) : l'outil ne le retrouverait pas",
			textutil.CleanText(doc.Name, 64), textutil.CleanText(c.Project, 64)), false
	}
	ps := run.Run(append(composeBase(c), "ps", "-q", "--all", c.Service), false, dockerx.DefaultRunTimeout)
	if ps.RC != 0 {
		return "docker compose ps a échoué : " + textutil.Truncate(textutil.Strip(ps.Err), 200), false
	}
	if f := textutil.Fields(ps.Out); len(f) == 0 || !(strings.HasPrefix(c.ID, f[0]) || strings.HasPrefix(f[0], c.ID)) {
		return "compose ne désigne pas ce conteneur pour ce service", false
	}
	return "", true
}

// Version lit la version du produit dans l'étiquette d'image (label), ou "".
func Version(run dockerx.Runner, image, label string) string {
	r := run.Run([]string{"docker", "image", "inspect", "--format", "{{json .Config.Labels}}", image}, false, dockerx.DefaultRunTimeout)
	if r.RC != 0 {
		return ""
	}
	var labels map[string]string
	if json.Unmarshal([]byte(strings.TrimSpace(r.Out)), &labels) != nil {
		return ""
	}
	return textutil.CleanText(labels[label], 64)
}

// Repo sépare une référence d'image en (hôte du registre, chemin sans étiquette ni digest).
func Repo(image string) (registry, repo string) {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	parts := strings.Split(image, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		registry, parts = parts[0], parts[1:]
	}
	repo = strings.Join(parts, "/")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return registry, repo
}

// Never : images qui ne se suivent pas par défaut, avec la raison.
func Never(image string) (string, bool) {
	_, repo := Repo(image)
	last := repo[strings.LastIndex(repo, "/")+1:]
	switch {
	case last == "watchtower":
		return "Watchtower : à retirer à la fin de la migration, pas à suivre", true
	case strings.Contains(last, "socket-proxy"):
		return "proxy du socket Docker : on n'y touche pas automatiquement", true
	}
	for _, db := range []string{"postgres", "postgis", "mysql", "mariadb", "mongo", "redis", "valkey", "keydb", "memcached"} {
		if last == db || last == db+"ql" {
			return "base de données : une montée de version peut exiger une migration, à faire à la main, avec sauvegarde", true
		}
	}
	return "", false
}

// known : dépôts GitHub des avis, pour des images courantes (clé : chemin d'image sans registre ni étiquette).
var known = map[string]string{
	"traefik":                            "traefik/traefik",
	"crowdsecurity/crowdsec":             "crowdsecurity/crowdsec",
	"portainer/portainer-ce":             "portainer/portainer",
	"portainer/portainer":                "portainer/portainer",
	"grafana/grafana":                    "grafana/grafana",
	"jellyfin/jellyfin":                  "jellyfin/jellyfin",
	"louislam/uptime-kuma":               "louislam/uptime-kuma",
	"vaultwarden/server":                 "dani-garcia/vaultwarden",
	"adguard/adguardhome":                "AdguardTeam/AdGuardHome",
	"henrygd/beszel":                     "henrygd/beszel",
	"henrygd/beszel-agent":               "henrygd/beszel",
	"immich-app/immich-server":           "immich-app/immich",
	"immich-app/immich-machine-learning": "immich-app/immich",
	"gethomepage/homepage":               "gethomepage/homepage",
	"wg-easy/wg-easy":                    "wg-easy/wg-easy",
	"pocket-id/pocket-id":                "pocket-id/pocket-id",
	"nextcloud":                          "nextcloud/server",
}

// SuggestRepo propose un dépôt GitHub pour les avis : (dépôt, certain). Une image publiée sur ghcr.io sous
// <propriétaire>/<nom> donne <propriétaire>/<nom> « à confirmer » ; sinon "".
func SuggestRepo(image string) (string, bool) {
	registry, repo := Repo(image)
	if r, ok := known[repo]; ok {
		return r, true
	}
	if registry == "ghcr.io" && strings.Count(repo, "/") == 1 {
		return repo, false
	}
	return "", false
}
