// Package selfupdate remplace le binaire en cours par celui de la dernière release GitHub.
//
// Seul le binaire est touché : ni la configuration, ni l'état, ni les conteneurs. Le fichier téléchargé est comparé au
// SHA256SUMS de la même release, vérifié (ELF de la bonne architecture) puis posé par renommage atomique.
package selfupdate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Limites de lecture : le binaire pèse quelques Mo, la liste des sommes quelques lignes.
const (
	maxBinary = 128 << 20
	maxSmall  = 1 << 20
)

// Fetch lit une URL https et renvoie le corps (au plus limit octets ; au-delà, c'est une erreur).
type Fetch func(url string, limit int64) ([]byte, error)

// Deps regroupe ce qui touche au monde extérieur (remplacé dans les tests).
type Deps struct {
	Fetch Fetch
	// Repo : « propriétaire/dépôt » des releases.
	Repo string
	// APIBase : racine de l'API GitHub ; DownloadBase : racine des téléchargements de releases.
	APIBase      string
	DownloadBase string
	GOOS, GOARCH string
	// Current est la version en cours (« v1.2.3 », ou « dev » / git describe pour une compilation locale).
	Current string
	// Target est le binaire à remplacer.
	Target string
}

// Result décrit ce qui s'est passé.
type Result struct {
	Latest   string
	Current  string
	UpToDate bool
	Updated  bool
}

var tagRe = regexp.MustCompile(`^v(\d{1,6})\.(\d{1,6})\.(\d{1,6})$`)

// ParseTag lit « vX.Y.Z » (rien d'autre : pas de pré-version, pas de suffixe).
func ParseTag(s string) ([3]int, bool) {
	m := tagRe.FindStringSubmatch(s)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func newer(a, b [3]int) bool { // a strictement plus récent que b
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// Latest renvoie l'étiquette de la dernière release publiée.
func Latest(d Deps) (string, error) {
	raw, err := d.Fetch(fmt.Sprintf("%s/repos/%s/releases/latest", d.APIBase, d.Repo), maxSmall)
	if err != nil {
		return "", fmt.Errorf("lecture de la dernière release : %w", err)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(raw, &rel); err != nil {
		return "", errors.New("réponse GitHub illisible")
	}
	if _, ok := ParseTag(rel.Tag); !ok {
		return "", errors.New("étiquette de release inattendue (attendu : vX.Y.Z)")
	}
	return rel.Tag, nil
}

// Run cherche la dernière release et, sauf check, remplace le binaire. Une version plus ancienne n'est jamais installée.
func Run(d Deps, check bool, log func(string)) (Result, error) {
	res := Result{Current: d.Current}
	if d.GOOS != "linux" || (d.GOARCH != "amd64" && d.GOARCH != "arm64") {
		return res, fmt.Errorf("plateforme non prise en charge : %s/%s (releases : linux amd64 et arm64)", d.GOOS, d.GOARCH)
	}
	tag, err := Latest(d)
	if err != nil {
		return res, err
	}
	res.Latest = tag
	cur, curOK := ParseTag(d.Current)
	want, _ := ParseTag(tag)
	if curOK && !newer(want, cur) {
		res.UpToDate = true
		log(fmt.Sprintf("déjà à jour : %s (dernière release : %s)", d.Current, tag))
		return res, nil
	}
	log(fmt.Sprintf("version en cours : %s ; dernière release : %s", d.Current, tag))
	if check {
		log("mise à jour disponible (relancer sans --check pour l'installer)")
		return res, nil
	}

	asset := "docker-update-linux-" + d.GOARCH
	base := fmt.Sprintf("%s/%s/releases/download/%s/", d.DownloadBase, d.Repo, tag)
	sums, err := d.Fetch(base+"SHA256SUMS", maxSmall)
	if err != nil {
		return res, fmt.Errorf("téléchargement de SHA256SUMS : %w", err)
	}
	want256, err := findSum(sums, asset)
	if err != nil {
		return res, err
	}
	bin, err := d.Fetch(base+asset, maxBinary)
	if err != nil {
		return res, fmt.Errorf("téléchargement de %s : %w", asset, err)
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want256 {
		return res, errors.New("somme SHA256 différente de SHA256SUMS : binaire refusé, rien n'est remplacé")
	}
	if err := checkELF(bin, d.GOARCH); err != nil {
		return res, err
	}
	if err := replace(d.Target, bin); err != nil {
		return res, err
	}
	res.Updated = true
	log(fmt.Sprintf("mis à jour : %s -> %s (%s)", d.Current, tag, d.Target))
	log("la configuration et les conteneurs n'ont pas été touchés ; le prochain passage utilisera la nouvelle version")
	log("conseil : `docker-update validate` puis, si demandé, `docker-update migrate`")
	return res, nil
}

// findSum cherche la somme du fichier dans un SHA256SUMS (format sha256sum : « somme  nom » ou « somme *nom »).
func findSum(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		if len(f[0]) != 64 {
			break
		}
		if _, err := hex.DecodeString(f[0]); err != nil {
			break
		}
		return strings.ToLower(f[0]), nil
	}
	return "", fmt.Errorf("%s absent ou illisible dans SHA256SUMS", name)
}

// checkELF refuse un fichier qui n'est pas un exécutable ELF 64 bits de l'architecture attendue.
func checkELF(b []byte, arch string) error {
	if len(b) < 20 || string(b[:4]) != "\x7fELF" || b[4] != 2 || b[5] != 1 {
		return errors.New("le fichier téléchargé n'est pas un exécutable ELF 64 bits : refusé")
	}
	machine := uint16(b[18]) | uint16(b[19])<<8
	if (arch == "amd64" && machine != 0x3e) || (arch == "arm64" && machine != 0xb7) {
		return errors.New("l'exécutable téléchargé n'a pas la bonne architecture : refusé")
	}
	return nil
}

// replace écrit le nouveau binaire à côté de la cible (même dossier, donc même système de fichiers), puis le renomme dessus.
// Un binaire en cours d'exécution peut être remplacé ainsi sous Linux ; en cas d'échec, l'ancien reste intact.
func replace(target string, bin []byte) error {
	if p, err := filepath.EvalSymlinks(target); err == nil {
		target = p
	}
	mode := os.FileMode(0o755)
	if fi, err := os.Stat(target); err == nil {
		mode = fi.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(target), ".docker-update-*.new")
	if err != nil {
		return fmt.Errorf("écriture à côté de %s impossible (droits ? relancer en root) : %w", target, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(bin); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}
	ok = true
	return nil
}

var client = &http.Client{
	Timeout: 2 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("redirection refusée")
		}
		return nil
	},
}

// HTTPFetch est le Fetch réel : https seulement, taille bornée, aucun jeton envoyé.
func HTTPFetch(url string, limit int64) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errors.New("URL non https refusée")
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "docker-update-selfupdate")
	req.Header.Set("Accept", "application/octet-stream, application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("réseau injoignable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("introuvable (HTTP 404 : pas encore de release ?)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("réponse HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("lecture interrompue")
	}
	if int64(len(b)) > limit {
		return nil, errors.New("réponse trop grande")
	}
	return b, nil
}
