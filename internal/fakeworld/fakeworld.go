// Package fakeworld est un hôte Docker minimal (registre, images, conteneurs, compose) qui implémente
// dockerx.Runner : de quoi rejouer les scénarios de mise à jour dans les tests, sans jamais lancer docker.
//
// C'est le portage de la classe World des tests Python. Une commande inattendue fait paniquer le test : c'est voulu,
// l'outil ne doit lancer que les commandes prévues.
package fakeworld

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Bloopps/docker-update/internal/dockerx"
	"github.com/Bloopps/docker-update/internal/textutil"
)

// Manifest : faux manifeste d'index ; son hachage est le digest que le « registre » annonce.
func Manifest(n int) []byte { return []byte(fmt.Sprintf("{\"schemaVersion\":2,\"n\":%d}\n", n)) }

// DG : digest du faux manifeste n.
func DG(n int) string {
	sum := sha256.Sum256(Manifest(n))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Container : un conteneur compose.
type Container struct {
	ID, ImageID, Status, Health string
}

// Up : un appel `compose up` (service, id de l'image).
type Up struct{ Service, Image string }

// Key désigne un service d'une pile compose.
type Key struct{ Dir, Service string }

// World est l'hôte simulé.
type World struct {
	Registry       map[string][]byte
	Images         map[string][]string // id -> ["repo@digest"]
	ByRefDigest    map[string]string   // "repo@digest" -> id
	Tags           map[string]string   // référence -> id
	Containers     map[Key]*Container
	Compose        map[string]map[string]string // dossier -> service -> référence
	Calls          [][]string
	Ups            []Up
	FailUp         map[string]bool
	Restarting     map[string]bool
	FailPull       map[string]bool
	Removed        []string
	KeepImages     map[string]bool // images que docker refuse de supprimer (encore utilisées)
	FailRegistry   map[string]bool
	Dry            bool
	SecretInConfig string
	Labels         map[string]map[string]any // id d'image -> étiquettes
	DigestVersions map[string]string         // digest -> version annoncée par l'image candidate
}

// New crée un hôte vide.
func New() *World {
	return &World{
		Registry: map[string][]byte{}, Images: map[string][]string{}, ByRefDigest: map[string]string{}, Tags: map[string]string{},
		Containers: map[Key]*Container{}, Compose: map[string]map[string]string{}, FailUp: map[string]bool{},
		Restarting: map[string]bool{}, FailPull: map[string]bool{}, KeepImages: map[string]bool{}, FailRegistry: map[string]bool{},
		SecretInConfig: "hunter2", Labels: map[string]map[string]any{}, DigestVersions: map[string]string{},
	}
}

type addOpts struct {
	absent, noDigests bool
	version           string
}

// AddOpt : option de Add.
type AddOpt func(*addOpts)

// Absent : le service existe dans le compose mais n'a pas de conteneur.
func Absent() AddOpt { return func(o *addOpts) { o.absent = true } }

// NoDigests : l'image locale n'a aucun digest (construite sur place).
func NoDigests() AddOpt { return func(o *addOpts) { o.noDigests = true } }

// WithVersion : étiquette de version de l'image en cours.
func WithVersion(v string) AddOpt { return func(o *addOpts) { o.version = v } }

// Add déclare un service : image locale, compose, registre, conteneur. Renvoie l'id de l'image locale.
func (w *World) Add(name, dir, service, ref string, local int, opts ...AddOpt) string {
	var o addOpts
	for _, f := range opts {
		f(&o)
	}
	repo, tag, _ := textutil.SplitRef(ref)
	iid := "id-" + name + "-old"
	if o.noDigests {
		w.Images[iid] = []string{}
	} else {
		w.Images[iid] = []string{repo + "@" + DG(local)}
	}
	w.Tags[ref] = iid
	if w.Compose[dir] == nil {
		w.Compose[dir] = map[string]string{}
	}
	w.Compose[dir][service] = ref
	w.Registry[repo+":"+tag] = Manifest(local)
	if o.version != "" {
		w.Labels[iid] = map[string]any{"org.opencontainers.image.version": o.version}
	}
	if !o.absent {
		w.Containers[Key{dir, service}] = &Container{ID: "cid-" + service, ImageID: iid, Status: "running"}
	}
	return iid
}

// DryRun : simulation.
func (w *World) DryRun() bool { return w.Dry }

// RunBytes : lecture du manifeste brut (buildx).
func (w *World) RunBytes(argv []string, timeout time.Duration) (int, []byte, string) {
	w.Calls = append(w.Calls, append([]string{}, argv...))
	if len(argv) < 6 || strings.Join(argv[:4], " ") != "docker buildx imagetools inspect" || argv[len(argv)-1] != "--raw" {
		panic(fmt.Sprintf("commande inattendue : %v", argv))
	}
	full := argv[4]
	if w.FailRegistry[full] {
		return 1, nil, "unauthorized"
	}
	raw, ok := w.Registry[full]
	if !ok {
		panic("référence absente du registre simulé : " + full)
	}
	return 0, raw, ""
}

func res(rc int, out, err string) dockerx.Result { return dockerx.Result{RC: rc, Out: out, Err: err} }

// Run : les commandes docker, compose et regctl.
func (w *World) Run(argv []string, mutating bool, timeout time.Duration) dockerx.Result {
	if mutating && w.Dry {
		return res(0, "", "")
	}
	w.Calls = append(w.Calls, append([]string{}, argv...))
	at := func(i int) string {
		if i < len(argv) {
			return argv[i]
		}
		return ""
	}
	switch {
	case at(0) == "docker" && at(1) == "compose":
		dir := ""
		for i, a := range argv {
			if a == "--project-directory" {
				dir = argv[i+1]
			}
		}
		sub := ""
		for _, a := range argv[2:] {
			if a == "ps" || a == "config" || a == "up" {
				sub = a
				break
			}
		}
		switch sub {
		case "ps":
			if c, ok := w.Containers[Key{dir, argv[len(argv)-1]}]; ok {
				return res(0, c.ID+"\n", "")
			}
			return res(0, "", "")
		case "config":
			svcs := map[string]any{}
			for s, r := range w.Compose[dir] {
				svcs[s] = map[string]any{"image": r, "environment": map[string]any{"TOKEN": w.SecretInConfig}}
			}
			b, _ := json.Marshal(map[string]any{"services": svcs})
			return res(0, string(b), "")
		}
		service := argv[len(argv)-1]
		iid := w.Tags[w.Compose[dir][service]]
		w.Ups = append(w.Ups, Up{service, iid})
		if w.FailUp[iid] {
			return res(1, "", "service unhealthy")
		}
		status := "running"
		if w.Restarting[iid] {
			status = "restarting"
		}
		w.Containers[Key{dir, service}] = &Container{ID: "cid-" + service, ImageID: iid, Status: status}
		return res(0, "", "")
	case at(0) == "docker" && at(1) == "inspect":
		cid := argv[len(argv)-1]
		for _, c := range w.Containers {
			if c.ID == cid {
				if strings.Contains(argv[3], "State.Status") {
					return res(0, c.Status+"|"+c.Health+"\n", "")
				}
				return res(0, c.ImageID+"\n", "")
			}
		}
		panic("conteneur inconnu : " + cid)
	case at(0) == "docker" && at(1) == "image" && at(2) == "rm":
		if w.KeepImages[argv[3]] {
			return res(1, "", "conflict: unable to delete: image is being used by stopped container")
		}
		w.Removed = append(w.Removed, argv[3])
		return res(0, "Deleted: "+argv[3], "")
	case at(0) == "docker" && at(1) == "image" && at(2) == "inspect":
		if strings.Contains(argv[4], "Config.Labels") {
			target := argv[len(argv)-1]
			iid := target
			if v, ok := w.ByRefDigest[target]; ok {
				iid = v
			}
			labels := w.Labels[iid]
			if labels == nil {
				labels = map[string]any{}
			}
			b, _ := json.Marshal(labels)
			return res(0, string(b), "")
		}
		imgs := w.Images[argv[len(argv)-1]]
		if imgs == nil {
			imgs = []string{}
		}
		b, _ := json.Marshal(imgs)
		return res(0, string(b), "")
	case at(0) == "regctl" && at(1) == "image":
		full := argv[3]
		if w.FailRegistry[full] {
			return res(1, "", "unauthorized")
		}
		raw, ok := w.Registry[full]
		if !ok {
			panic("référence absente du registre simulé : " + full)
		}
		if len(raw) > 0 {
			sum := sha256.Sum256(raw)
			return res(0, "sha256:"+hex.EncodeToString(sum[:])+"\n", "")
		}
		return res(0, "\n", "")
	case at(0) == "docker" && at(1) == "pull":
		target := argv[2]
		digest := strings.SplitN(target, "@", 2)[1]
		if w.FailPull[digest] {
			return res(1, "", "pull failed")
		}
		iid := "id-" + textutil.Slice(digest, 7, 19)
		w.Images[iid] = []string{target}
		w.ByRefDigest[target] = iid
		if v := w.DigestVersions[digest]; v != "" {
			w.Labels[iid] = map[string]any{"org.opencontainers.image.version": v}
		}
		return res(0, "", "")
	case at(0) == "docker" && at(1) == "tag":
		src := argv[2]
		iid := src
		if v, ok := w.ByRefDigest[src]; ok {
			iid = v
		}
		w.Tags[argv[3]] = iid
		return res(0, "", "")
	}
	panic(fmt.Sprintf("commande inattendue : %v", argv))
}

// Cmds : les commandes enregistrées qui commencent par prefix.
func (w *World) Cmds(prefix ...string) [][]string {
	var out [][]string
	for _, c := range w.Calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], "\x00") == strings.Join(prefix, "\x00") {
			out = append(out, c)
		}
	}
	return out
}
