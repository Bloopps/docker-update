// Faux `docker` pour les tests de bout en bout : l'état du monde est dans le fichier $SHIM_STATE (JSON).
// Il refuse toute commande ou tout argument que le vrai programme n'est pas censé envoyer.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

type container struct {
	ID      string `json:"id"`
	ImageID string `json:"image_id"`
	Status  string `json:"status"`
}

type world struct {
	Registry       map[string]string            `json:"registry"`
	Images         map[string][]string          `json:"images"`
	Tags           map[string]string            `json:"tags"`
	By             map[string]string            `json:"by"`
	Containers     map[string]*container        `json:"containers"`
	Compose        map[string]map[string]string `json:"compose"`
	FailUp         []string                     `json:"fail_up"`
	Labels         map[string]map[string]string `json:"labels"`
	DigestVersions map[string]string            `json:"digest_versions"`
	Log            [][]string                   `json:"log"`
	Removed        []string                     `json:"removed,omitempty"`
}

var (
	path = os.Getenv("SHIM_STATE")
	w    world
	args = os.Args[1:]
)

func save() {
	b, err := json.Marshal(&w)
	if err == nil {
		err = os.WriteFile(path, b, 0o600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "faux docker : état non écrit :", err)
		os.Exit(3)
	}
}

func die(rc int, format string, a ...any) {
	save()
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(rc)
}

func index(x string) int { return slices.Index(args, x) }

func after(flag string) string {
	i := index(flag)
	if i < 0 || i+1 >= len(args) {
		die(2, "option attendue : %s", flag)
	}
	return args[i+1]
}

func main() {
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &w) != nil {
		fmt.Fprintln(os.Stderr, "faux docker : SHIM_STATE illisible")
		os.Exit(3)
	}
	w.Log = append(w.Log, append([]string{}, args...))
	last := ""
	if len(args) > 0 {
		last = args[len(args)-1]
	}
	switch {
	case len(args) > 0 && args[0] == "compose":
		compose(last)
	case len(args) > 0 && args[0] == "inspect":
		var c *container
		for _, v := range w.Containers {
			if v.ID == last {
				c = v
			}
		}
		if c == nil {
			die(1, "Error: No such object: %s", last)
		}
		if strings.Contains(args[2], "State.Status") {
			fmt.Print(c.Status + "|\n")
		} else {
			fmt.Print(c.ImageID + "\n")
		}
	case len(args) > 1 && args[0] == "image" && args[1] == "inspect":
		if strings.Contains(args[3], "Config.Labels") {
			id := last
			if v, ok := w.By[last]; ok {
				id = v
			}
			labels := w.Labels[id]
			if labels == nil {
				labels = map[string]string{}
			}
			out, _ := json.Marshal(labels)
			fmt.Print(string(out))
		} else {
			refs := w.Images[last]
			if refs == nil {
				refs = []string{}
			}
			out, _ := json.Marshal(refs)
			fmt.Print(string(out))
		}
	case len(args) > 2 && args[0] == "buildx" && args[1] == "imagetools" && args[2] == "inspect":
		if last != "--raw" {
			die(2, "--raw attendu")
		}
		m, ok := w.Registry[args[3]]
		if !ok {
			die(1, "not found: %s", args[3])
		}
		fmt.Print(m)
	case len(args) > 0 && args[0] == "pull":
		repo, digest, ok := strings.Cut(args[1], "@")
		if !ok || repo == "" || len(digest) < 19 {
			die(2, "pull attendu par digest : %s", args[1])
		}
		id := "id-" + digest[7:19]
		w.Images[id] = []string{args[1]}
		w.By[args[1]] = id
		if v := w.DigestVersions[digest]; v != "" {
			if w.Labels == nil {
				w.Labels = map[string]map[string]string{}
			}
			w.Labels[id] = map[string]string{"org.opencontainers.image.version": v}
		}
	case len(args) > 1 && args[0] == "image" && args[1] == "rm":
		if slices.Contains(args, "-f") || slices.Contains(args, "--force") {
			die(2, "le nettoyage ne force jamais")
		}
		for _, c := range w.Containers {
			if c.ImageID == args[2] {
				die(1, "Error: conflict: unable to delete %s: image is being used by running container", args[2])
			}
		}
		w.Removed = append(w.Removed, args[2])
	case len(args) > 0 && args[0] == "tag":
		src := args[1]
		if v, ok := w.By[src]; ok {
			src = v
		}
		w.Tags[args[2]] = src
	default:
		die(2, "commande inattendue : %q", args)
	}
	save()
}

func compose(svc string) {
	dir := after("--project-directory")
	sub := ""
	for _, a := range args[1:] {
		if a == "ps" || a == "config" || a == "up" {
			sub = a
			break
		}
	}
	key := dir + "|" + svc
	switch sub {
	case "ps":
		if c, ok := w.Containers[key]; ok {
			fmt.Print(c.ID + "\n")
		}
	case "config":
		services := map[string]any{}
		for s, ref := range w.Compose[dir] {
			services[s] = map[string]any{"image": ref, "environment": map[string]string{"T": "hunter2"}}
		}
		out, _ := json.Marshal(map[string]any{"services": services})
		fmt.Print(string(out))
	case "up":
		for _, f := range []string{"-d", "--no-deps", "--wait"} {
			if index(f) < 0 {
				die(2, "option attendue : %s", f)
			}
		}
		if after("--pull") != "never" {
			die(2, "--pull never attendu")
		}
		id := w.Tags[w.Compose[dir][svc]]
		if slices.Contains(w.FailUp, id) {
			die(1, "service unhealthy")
		}
		c := w.Containers[key]
		if c == nil {
			die(1, "no such service: %s", svc)
		}
		c.ImageID, c.Status = id, "running"
	default:
		die(2, "sous-commande compose inattendue : %q", args)
	}
}
