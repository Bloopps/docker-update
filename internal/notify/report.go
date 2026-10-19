package notify

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/textutil"
)

// Section : une rubrique du rapport, du plus important au moins important.
type Section struct {
	Key      string
	Icon     string
	Heading  string
	Singular string
	Plural   string
}

// Sections du rapport, dans l'ordre d'affichage.
var Sections = []Section{
	{"echec", "🚨", "Échecs de mise à jour", "échec", "échecs"},
	{"secu", "⚠️", "Sécurité (avis)", "alerte de sécurité", "alertes de sécurité"},
	{"notes", "📝", "Notes de release (alertes seulement, délai inchangé)", "note de release", "notes de release"},
	{"erreur", "⚠️", "Erreurs de vérification", "erreur", "erreurs"},
	{"maj", "✅", "Mises à jour réussies", "mise à jour", "mises à jour"},
	{"pret", "📦", "Nouvelles versions prêtes (à appliquer avec `docker-update now <service> --yes`)", "version prête", "versions prêtes"},
	{"info", "ℹ️", "Infos", "info", "infos"},
	{"etat", "📋", "État des services (scan manuel)", "service scanné", "services scannés"},
}

// tableSpec : en-tête et largeur maximale de chaque colonne d'un tableau.
type tableSpec struct {
	head   []string
	limits []int
}

var tables = map[string]tableSpec{
	"pret": {[]string{"service", "image", "vue depuis"}, []int{28, 14, 12}},
	"etat": {[]string{"service", "état", "détail"}, []int{28, 10, 52}},
}

var levelRank = map[string]int{"info": 0, "ok": 1, "warn": 2, "error": 3}

// Limites du rapport.
const (
	ReportBudget = 1650 // caractères : sous la limite de Discord (1900) avec le titre
	ReportLines  = 5    // lignes affichées par élément
	ReportLine   = 300  // caractères par ligne
)

// Item est un élément d'une rubrique.
type Item struct {
	Title, Text, Level string
}

// Report regroupe tout ce qu'un passage a à dire en UN message : rubriques par importance, tableaux des versions prêtes.
type Report struct {
	items  map[string][]Item
	tables map[string][][]string
}

// NewReport crée un rapport vide.
func NewReport() *Report {
	return &Report{items: map[string][]Item{}, tables: map[string][][]string{}}
}

// Add ajoute un élément à une rubrique.
func (r *Report) Add(kind, title, text, level string) {
	r.items[kind] = append(r.items[kind], Item{title, text, level})
}

// AddRow ajoute une ligne au tableau d'une rubrique.
func (r *Report) AddRow(kind string, cells ...string) {
	r.tables[kind] = append(r.tables[kind], cells)
}

// AddReady ajoute une version prête au tableau « pret ».
func (r *Report) AddReady(service, image, since string) { r.AddRow("pret", service, image, since) }

// Empty dit si le rapport n'a rien à dire.
func (r *Report) Empty() bool {
	for _, v := range r.items {
		if len(v) > 0 {
			return false
		}
	}
	for _, v := range r.tables {
		if len(v) > 0 {
			return false
		}
	}
	return true
}

// Counts donne le nombre d'éléments par rubrique (les rubriques vides sont absentes).
func (r *Report) Counts() map[string]int {
	c := map[string]int{}
	for _, s := range Sections {
		if n := len(r.items[s.Key]) + len(r.tables[s.Key]); n > 0 {
			c[s.Key] = n
		}
	}
	return c
}

// Level est le pire niveau des éléments ("info" s'il n'y en a pas).
func (r *Report) Level() string {
	worst := "info"
	for _, s := range Sections {
		for _, it := range r.items[s.Key] {
			if levelRank[it.Level] > levelRank[worst] {
				worst = it.Level
			}
		}
	}
	return worst
}

// Title est le titre du message : « docker-update : 1 échec · 2 versions prêtes ».
func (r *Report) Title() string {
	var parts []string
	counts := r.Counts()
	for _, s := range Sections {
		if n := counts[s.Key]; n > 0 {
			word := s.Plural
			if n == 1 {
				word = s.Singular
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, word))
		}
	}
	return "docker-update : " + strings.Join(parts, " · ")
}

// esc nettoie un texte et neutralise les mentions, quel que soit le format.
func esc(value string, limit int) string {
	return strings.ReplaceAll(textutil.CleanText(value, limit), "@", "@\u200b")
}

func padRight(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// Render écrit le texte du rapport, borné par budget (en caractères). Ce qui ne tient pas est compté, jamais coupé
// au milieu d'une ligne.
func (r *Report) Render(fence bool, budget int) string {
	var out []string
	used := 0
	hidden := 0
	put := func(lines []string) bool {
		size := 0
		for _, l := range lines {
			size += utf8.RuneCountInString(l) + 1
		}
		if used+size > budget {
			return false
		}
		out = append(out, lines...)
		used += size
		return true
	}
	for _, s := range Sections {
		items := r.items[s.Key]
		rows := r.tables[s.Key]
		if len(items) == 0 && len(rows) == 0 {
			continue
		}
		block := []string{s.Heading + " :"}
		if fence {
			block = []string{s.Icon + " **" + s.Heading + "**"}
		}
		if !put(block) {
			hidden += len(items) + len(rows)
			continue
		}
		for _, it := range items {
			first := "- " + esc(it.Title, 120)
			if fence {
				first = "• **" + esc(it.Title, 120) + "**"
			}
			lines := []string{first}
			shown := 0
			for _, extra := range strings.Split(it.Text, "\n") {
				if strings.TrimSpace(extra) == "" {
					continue
				}
				if shown == ReportLines {
					break
				}
				lines = append(lines, "  "+esc(extra, ReportLine))
				shown++
			}
			if !put(lines) {
				hidden++
			}
		}
		if len(rows) > 0 {
			spec := tables[s.Key]
			body := make([][]string, len(rows))
			for i, row := range rows {
				cells := make([]string, len(spec.limits))
				for j := range cells {
					if j < len(row) {
						cells[j] = esc(row[j], spec.limits[j])
					}
				}
				body[i] = cells
			}
			table := func(n int) []string {
				data := append([][]string{spec.head}, body[:n]...)
				widths := make([]int, len(spec.head))
				for _, row := range data {
					for i, c := range row {
						widths[i] = max(widths[i], utf8.RuneCountInString(c))
					}
				}
				var lines []string
				for _, row := range data {
					cells := make([]string, len(row))
					for i, c := range row {
						cells[i] = padRight(c, widths[i])
					}
					lines = append(lines, strings.TrimRight(strings.Join(cells, "  "), " \t\n\r\f\v"))
				}
				if fence {
					return append(append([]string{"```"}, lines...), "```")
				}
				return lines
			}
			n := len(body)
			for n > 0 && !put(table(n)) {
				n--
			}
			hidden += len(body) - n
		}
	}
	if hidden > 0 {
		out = append(out, fmt.Sprintf("… %d élément(s) non affiché(s) : voir le journal (`journalctl -u docker-update`).", hidden))
	}
	return strings.Join(out, "\n")
}
