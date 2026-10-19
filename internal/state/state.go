// Package state gère l'état mémorisé entre deux passages (digests vus, pauses, erreurs, caches GitHub, alertes).
//
// Le format est celui de la version Python : un objet JSON indenté d'un espace, clés triées, écriture atomique. Les
// structures déclarent leurs champs dans l'ordre alphabétique de leurs clés JSON pour que le fichier écrit par Go
// soit identique, octet pour octet, à celui du Python (hors clés vides omises, voir Feed).
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/textutil"
)

// Error est une erreur d'état (état illisible ou invalide) : code de sortie 1.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

// Image est ce qu'on sait d'une image candidate : digest vu, depuis quand, avertie, déjà essayée sans succès.
type Image struct {
	Digest    string `json:"digest"`
	Failed    bool   `json:"failed"`
	FirstSeen int64  `json:"first_seen"`
	Notified  bool   `json:"notified"`
}

// Feed est le cache d'une liste GitHub (avis ou releases) pour un dépôt. Les champs vides (err, etag, full, ok)
// sont omis : en Python ils sont absents ou à zéro, ce que les deux versions lisent de la même façon.
type Feed[T any] struct {
	Err     int    `json:"err,omitempty"` // dernier code d'échec (0 : réseau)
	Etag    string `json:"etag,omitempty"`
	Fetched int64  `json:"fetched"`
	Full    int64  `json:"full,omitempty"` // releases : dernière synchronisation complète
	Items   []T    `json:"items"`
	Ok      bool   `json:"ok,omitempty"`
}

// State est l'état complet. Les champs sont dans l'ordre alphabétique de leurs clés JSON.
type State struct {
	Advisories map[string]*Feed[textutil.Advisory] `json:"advisories"`
	Alerts     map[string]int64                    `json:"alerts"`
	Errors     map[string]string                   `json:"errors"`
	Holds      []string                            `json:"holds"`
	Images     map[string]*Image                   `json:"images"`
	Releases   map[string]*Feed[textutil.Release]  `json:"releases"`

	path string
}

func (s *State) init() {
	if s.Advisories == nil {
		s.Advisories = map[string]*Feed[textutil.Advisory]{}
	}
	if s.Alerts == nil {
		s.Alerts = map[string]int64{}
	}
	if s.Errors == nil {
		s.Errors = map[string]string{}
	}
	if s.Holds == nil {
		s.Holds = []string{}
	}
	if s.Images == nil {
		s.Images = map[string]*Image{}
	}
	if s.Releases == nil {
		s.Releases = map[string]*Feed[textutil.Release]{}
	}
}

// Load lit l'état ; un fichier absent donne un état vide.
func Load(path string) (*State, error) {
	s := &State{path: path}
	s.init()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	unreadable := &Error{fmt.Sprintf("état illisible : %s (le déplacer pour repartir de zéro)", path)}
	if err != nil {
		return nil, unreadable
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		var generic any
		if json.Unmarshal(data, &generic) == nil {
			return nil, &Error{fmt.Sprintf("état invalide : %s", path)} // JSON valide, mais pas un objet
		}
		return nil, unreadable
	}
	for _, f := range []struct {
		key string
		dst any
	}{{"images", &s.Images}, {"holds", &s.Holds}, {"errors", &s.Errors}, {"advisories", &s.Advisories}, {"alerts", &s.Alerts}, {"releases", &s.Releases}} {
		raw, ok := top[f.key]
		if !ok {
			continue
		}
		// « null » n'est ni un objet ni une liste : refusé comme tout mauvais type (comme en Python).
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, f.dst) != nil {
			return nil, &Error{fmt.Sprintf("état invalide (%s) : %s", f.key, path)}
		}
	}
	s.init()
	// Un élément « null » dans une table (images, caches) est un état invalide.
	for _, v := range s.Images {
		if v == nil {
			return nil, &Error{fmt.Sprintf("état invalide (images) : %s", path)}
		}
	}
	for _, v := range s.Advisories {
		if v == nil {
			return nil, &Error{fmt.Sprintf("état invalide (advisories) : %s", path)}
		}
		if v.Items == nil {
			v.Items = []textutil.Advisory{}
		}
	}
	for _, v := range s.Releases {
		if v == nil {
			return nil, &Error{fmt.Sprintf("état invalide (releases) : %s", path)}
		}
		if v.Items == nil {
			v.Items = []textutil.Release{}
		}
	}
	return s, nil
}

// Path est le fichier d'état.
func (s *State) Path() string { return s.path }

// Get renvoie l'enregistrement d'une image, ou nil.
func (s *State) Get(ref string) *Image { return s.Images[ref] }

// Put enregistre une image.
func (s *State) Put(ref string, img *Image) { s.Images[ref] = img }

// Drop oublie une image.
func (s *State) Drop(ref string) { delete(s.Images, ref) }

// Marshal produit le JSON du fichier d'état : indenté d'un espace, clés triées, sans retour à la ligne final, ASCII
// seulement (comme json.dump de Python).
func (s *State) Marshal() ([]byte, error) {
	s.init()
	for _, f := range s.Advisories {
		if f.Items == nil {
			f.Items = []textutil.Advisory{}
		}
	}
	for _, f := range s.Releases {
		if f.Items == nil {
			f.Items = []textutil.Release{}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return escapeNonASCII(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// escapeNonASCII écrit \uXXXX (minuscules, paires de substitution) pour tout caractère non ASCII ; en JSON, ils ne
// peuvent se trouver que dans des chaînes.
func escapeNonASCII(b []byte) []byte {
	var out bytes.Buffer
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r < 0x80:
			out.WriteByte(b[0])
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(&out, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
		b = b[size:]
	}
	return out.Bytes()
}

// Save écrit l'état de façon atomique : fichier temporaire dans le même dossier, droits 600, renommage.
func (s *State) Save() error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	ok = true
	return nil
}

// HasHold dit si un nom (service ou groupe) est en pause.
func (s *State) HasHold(name string) bool {
	for _, h := range s.Holds {
		if h == name {
			return true
		}
	}
	return false
}
