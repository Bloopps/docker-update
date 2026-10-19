package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Bloopps/docker-update/internal/config"
)

// ── TestReport ──────────────────────────────────────────────────────────────

func full() *Report {
	r := NewReport()
	r.Add("maj", "homepage → a1b2c3d4e5f6", "", "ok")
	r.Add("notes", "Notes de release à lire : portainer 2.44.0 → 2.45.1", "Motifs : CVE-2026-1111\nLien : https://x", "warn")
	r.Add("echec", "tirelire (web)", "web ne repart pas\nPas touchés : admin", "error")
	r.Add("secu", "Correctif de sécurité détecté : traefik", "GHSA-2222-3333-4444 corrigé dans 3.7.14", "warn")
	r.AddReady("immich", "3f9a2b1c0d4e", "2 j")
	r.AddReady("wg-easy", "0123456789ab", "1 j")
	return r
}

func TestReportEmpty(t *testing.T) {
	r := NewReport()
	if !r.Empty() || len(r.Counts()) != 0 || r.Level() != "info" {
		t.Errorf("rapport vide : %v %v %v", r.Empty(), r.Counts(), r.Level())
	}
	r.AddReady("a", "b", "c")
	if r.Empty() {
		t.Error("une ligne de tableau n'est pas « vide »")
	}
}

func TestReportTitleCountsWithPlurals(t *testing.T) {
	want := "docker-update : 1 échec · 1 alerte de sécurité · 1 note de release · 1 mise à jour · 2 versions prêtes"
	if got := full().Title(); got != want {
		t.Errorf("titre : %q", got)
	}
	r := NewReport()
	r.Add("echec", "x", "", "error")
	r.Add("echec", "y", "", "error")
	if got := r.Title(); got != "docker-update : 2 échecs" {
		t.Errorf("titre : %q", got)
	}
}

func TestReportLevelIsTheWorstOne(t *testing.T) {
	r := NewReport()
	r.AddReady("a", "b", "c")
	for _, step := range []struct{ kind, level, want string }{{"", "", "info"}, {"maj", "ok", "ok"}, {"notes", "warn", "warn"}, {"echec", "error", "error"}} {
		if step.kind != "" {
			r.Add(step.kind, "x", "", step.level)
		}
		if got := r.Level(); got != step.want {
			t.Errorf("après %q : %s ; attendu %s", step.kind, got, step.want)
		}
	}
}

func TestReportSectionsComeInOrderOfImportance(t *testing.T) {
	text := full().Render(true, ReportBudget)
	last := -1
	for _, h := range []string{"Échecs de mise à jour", "Sécurité (avis)", "Notes de release (alertes seulement", "Mises à jour réussies", "Nouvelles versions prêtes"} {
		i := strings.Index(text, h)
		if i < 0 || i < last {
			t.Fatalf("%q mal placé (%d après %d) :\n%s", h, i, last, text)
		}
		last = i
	}
}

func TestReportReadyVersionsAreATable(t *testing.T) {
	text := full().Render(true, ReportBudget)
	if want := "```\nservice  image         vue depuis\nimmich   3f9a2b1c0d4e  2 j\nwg-easy  0123456789ab  1 j\n```"; !strings.Contains(text, want) {
		t.Errorf("tableau absent :\n%s", text)
	}
	plain := full().Render(false, ReportBudget)
	if strings.Contains(plain, "```") || strings.Contains(plain, "**") || !strings.Contains(plain, "immich   3f9a2b1c0d4e  2 j") {
		t.Errorf("texte brut :\n%s", plain)
	}
}

func TestReportAnItemShowsItsFirstLinesOnlyAndBounded(t *testing.T) {
	r := NewReport()
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("ligne %d %s", i, strings.Repeat("x", 500)))
	}
	r.Add("secu", "t", strings.Join(lines, "\n"), "info")
	text := r.Render(true, ReportBudget)
	if got := strings.Count(text, "ligne "); got != ReportLines {
		t.Errorf("%d lignes affichées ; attendu %d", got, ReportLines)
	}
	for _, l := range strings.Split(text, "\n") {
		if utf8.RuneCountInString(l) > ReportLine+10 {
			t.Errorf("ligne de %d caractères", utf8.RuneCountInString(l))
		}
	}
}

func TestReportNoMentionAndNoControlCharacters(t *testing.T) {
	r := NewReport()
	r.Add("secu", "@everyone <@&1> "+"\x1b[31m", "texte @here"+"\x00"+"\n@everyone", "warn")
	for _, fence := range []bool{true, false} {
		text := r.Render(fence, ReportBudget)
		for _, bad := range []string{"@everyone", "@here", "<@&", "\x1b", "\x00"} {
			if strings.Contains(text, bad) {
				t.Errorf("%q présent (fence=%v) : %q", bad, fence, text)
			}
		}
		if !strings.Contains(text, "@"+zeroWidth+"everyone") {
			t.Errorf("« @ » doit être neutralisé : %q", text)
		}
	}
}

const zeroWidth = "\xe2\x80\x8b"

func TestReportBudgetIsRespectedAndTheImportantPartSurvives(t *testing.T) {
	r := NewReport()
	r.Add("echec", "tirelire", "ne repart pas", "error")
	for i := 0; i < 80; i++ {
		r.Add("info", fmt.Sprintf("info %d", i), strings.Repeat("détail ", 20), "info")
		r.AddReady(fmt.Sprintf("app-%02d", i), "0123456789ab", "1 j")
	}
	text := r.Render(true, ReportBudget)
	if n := utf8.RuneCountInString(text); n > ReportBudget+120 {
		t.Errorf("%d caractères", n)
	}
	if !strings.Contains(text, "Échecs de mise à jour") || !strings.Contains(text, "```\nservice") {
		t.Errorf("la partie importante doit survivre :\n%s", text)
	}
	if !regexp.MustCompile(`… \d+ élément\(s\) non affiché\(s\)`).MatchString(text) {
		t.Errorf("le nombre d'éléments cachés est indiqué :\n%s", text)
	}
	if strings.Count(text, "```")%2 != 0 {
		t.Error("le tableau est toujours refermé")
	}
}

func TestReportAHugeTableIsCutByRowsNotByCharacters(t *testing.T) {
	r := NewReport()
	for i := 0; i < 300; i++ {
		r.AddReady(fmt.Sprintf("application-numero-%d", i), "0123456789ab", "12 j")
	}
	text := r.Render(true, ReportBudget)
	if n := utf8.RuneCountInString(text); n > ReportBudget+120 {
		t.Errorf("%d caractères", n)
	}
	lines := strings.Split(strings.TrimRight(text, "\n "), "\n")
	if !strings.HasPrefix(lines[len(lines)-1], "… ") {
		t.Errorf("le nombre de lignes cachées est indiqué : %q", lines[len(lines)-1])
	}
	for _, l := range lines {
		if !(strings.HasPrefix(l, "application") || strings.TrimSpace(l) == "" || strings.ContainsRune("`📦s…", []rune(l)[0])) {
			t.Errorf("ligne de tableau coupée : %q", l)
		}
		if strings.HasPrefix(l, "application") && !strings.HasSuffix(l, "12 j") {
			t.Errorf("ligne de tableau coupée : %q", l)
		}
	}
}

func TestReportColumnsAreMeasuredInCharacters(t *testing.T) {
	r := NewReport()
	r.AddReady("éééé", "1", "x")
	r.AddReady("a", "2", "y")
	want := "```\nservice  image  vue depuis\néééé     1      x\na        2      y\n```"
	if got := r.Render(true, ReportBudget); !strings.Contains(got, want) {
		t.Errorf("colonnes :\n%s", got)
	}
	// État des services : trois colonnes, bornées par colonne.
	r = NewReport()
	r.AddRow("etat", strings.Repeat("s", 40), "à jour", strings.Repeat("d", 80))
	got := r.Render(false, ReportBudget)
	if !strings.Contains(got, strings.Repeat("s", 28)+"  ") || strings.Contains(got, strings.Repeat("s", 29)) || strings.Contains(got, strings.Repeat("d", 53)) {
		t.Errorf("largeurs maximales :\n%s", got)
	}
}

// ── TestInfra : notifications ───────────────────────────────────────────────

type sent struct {
	url     string
	body    []byte
	headers map[string]string
}

func writeURLFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "u")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func recorder(out *[]sent) PostFunc {
	return func(url string, body []byte, headers map[string]string) error {
		*out = append(*out, sent{url, body, headers})
		return nil
	}
}

func TestNotifierDiscordPayloadAndSecretNeverLogged(t *testing.T) {
	uf := writeURLFile(t, "https://discord.test/api/webhooks/1/TOP-SECRET\n")
	var got []sent
	var logs []string
	n := &Notifier{URLFile: uf, Format: FormatDiscord, Log: func(s string) { logs = append(logs, s) }, Post: recorder(&got)}
	n.Send("Titre", strings.Repeat("x", 5000), "error")
	var body struct {
		Content         string         `json:"content"`
		AllowedMentions map[string]any `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(got[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if parse, ok := body.AllowedMentions["parse"].([]any); !ok || len(parse) != 0 || len(body.AllowedMentions) != 1 {
		t.Errorf("aucune mention possible : %v", body.AllowedMentions)
	}
	if utf8.RuneCountInString(body.Content) != DiscordMax {
		t.Errorf("contenu de %d caractères", utf8.RuneCountInString(body.Content))
	}
	if !strings.HasPrefix(body.Content, "🚨 **Titre**\nxxx") {
		t.Errorf("contenu : %.30q", body.Content)
	}
	if got[0].url != "https://discord.test/api/webhooks/1/TOP-SECRET" {
		t.Errorf("url : %q", got[0].url)
	}
	if got[0].headers["Content-Type"] != "application/json" || !strings.HasPrefix(got[0].headers["User-Agent"], "docker-update") {
		t.Errorf("en-têtes : %v", got[0].headers)
	}
	boom := func(u string, b []byte, h map[string]string) error {
		return &url.Error{Op: "Post", URL: u, Err: errors.New("échec vers " + u)}
	}
	(&Notifier{URLFile: uf, Format: FormatDiscord, Log: func(s string) { logs = append(logs, s) }, Post: boom}).Send("t", "m", "info")
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "non envoyée") {
		t.Errorf("l'échec doit être journalisé : %q", joined)
	}
	if strings.Contains(joined, "TOP-SECRET") || strings.Contains(joined, "discord.test") {
		t.Errorf("l'URL ne doit jamais apparaître : %q", joined)
	}
}

func TestNotifierErrorKinds(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&HTTPStatusError{404}, "HTTPError"},
		{fmt.Errorf("enveloppe : %w", &HTTPStatusError{500}), "HTTPError"},
		{&url.Error{Op: "Post", URL: "https://secret/x", Err: errors.New("connection refused")}, "URLError"},
		{errors.New("https://secret/x"), "errorString"},
	} {
		if got := ErrorKind(c.err); got != c.want {
			t.Errorf("%v : %s ; attendu %s", c.err, got, c.want)
		}
	}
}

func TestNotifierWithoutURLAndOtherFormats(t *testing.T) {
	var got []sent
	(&Notifier{URLFile: "", Format: FormatDiscord, Log: func(string) {}, Post: recorder(&got)}).Send("t", "m", "info")
	(&Notifier{URLFile: "/nonexistent", Format: FormatDiscord, Log: func(string) {}, Post: recorder(&got)}).Send("t", "m", "info")
	(&Notifier{URLFile: writeURLFile(t, "  \n"), Format: FormatDiscord, Log: func(string) {}, Post: recorder(&got)}).Send("t", "m", "info")
	if len(got) != 0 {
		t.Fatalf("rien ne doit partir sans URL : %v", got)
	}
	uf := writeURLFile(t, "https://x.test/t")
	(&Notifier{URLFile: uf, Format: FormatNtfy, Log: func(string) {}, Post: recorder(&got)}).Send("Titre é", "corps", "error")
	(&Notifier{URLFile: uf, Format: FormatNtfy, Log: func(string) {}, Post: recorder(&got)}).Send("t", "corps", "ok")
	(&Notifier{URLFile: uf, Format: FormatJSON, Log: func(string) {}, Post: recorder(&got)}).Send("t", "m", "info")
	if string(got[0].body) != "corps" || got[0].headers["Priority"] != "high" || got[0].headers["Title"] != "Titre ?" {
		t.Errorf("ntfy : %s %v", got[0].body, got[0].headers)
	}
	if got[1].headers["Priority"] != "default" {
		t.Errorf("ntfy normal : %v", got[1].headers)
	}
	var j map[string]string
	if err := json.Unmarshal(got[2].body, &j); err != nil || j["level"] != "info" || j["title"] != "t" || j["text"] != "m" {
		t.Errorf("json : %s %v", got[2].body, err)
	}
	if string(got[2].body) != `{"title":"t","text":"m","level":"info"}` {
		t.Errorf("ordre des clés : %s", got[2].body)
	}
}

func TestNotifierDryRunOnlyLogs(t *testing.T) {
	var got []sent
	var logs []string
	n := &Notifier{URLFile: writeURLFile(t, "https://x.test/t"), Format: FormatDiscord, DryRun: true, Log: func(s string) { logs = append(logs, s) }, Post: recorder(&got)}
	n.Send("Titre", "m", "info")
	if len(got) != 0 || !slices.Equal(logs, []string{"[simulation] notification : Titre"}) {
		t.Errorf("%v %v", got, logs)
	}
}

func TestNotifierHTMLIsNotEscaped(t *testing.T) {
	var got []sent
	n := &Notifier{URLFile: writeURLFile(t, "https://x.test/t"), Format: FormatJSON, Log: func(string) {}, Post: recorder(&got)}
	n.Send("a<b>", "x&y", "info")
	if !strings.Contains(string(got[0].body), "a<b>") || !strings.Contains(string(got[0].body), "x&y") {
		t.Errorf("%s", got[0].body)
	}
}

// ── TestSetWebhook (partie logique) ─────────────────────────────────────────

const goodURL = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-abcDEF"

func setWebhook(t *testing.T, target, format, input string, dry bool) (string, error) {
	t.Helper()
	var logs []string
	err := SetWebhook(target, format, dry, func() (string, error) { return strings.SplitAfter(input, "\n")[0], nil }, func(s string) { logs = append(logs, s) })
	return strings.Join(logs, "\n"), err
}

func TestSetWebhookURLIsWrittenWith600AndNeverPrinted(t *testing.T) {
	target := filepath.Join(t.TempDir(), "etc", "discord_webhook")
	out, err := setWebhook(t, target, "discord", goodURL+"\n", false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	if string(b) != goodURL+"\n" {
		t.Errorf("contenu : %q", b)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o600 {
		t.Errorf("droits %o", info.Mode().Perm())
	}
	if strings.Contains(out, "AbCdEfGh") || strings.Contains(out, "123456789012345678") || !strings.Contains(out, "notify-test") {
		t.Errorf("sortie : %q", out)
	}
	entries, _ := os.ReadDir(filepath.Dir(target))
	if len(entries) != 1 || entries[0].Name() != "discord_webhook" {
		t.Errorf("aucun fichier temporaire : %v", entries)
	}
	n := &Notifier{URLFile: target, Format: FormatDiscord, Log: func(string) {}}
	var got []sent
	n.Post = recorder(&got)
	n.Send("t", "x", "info")
	if got[0].url != goodURL {
		t.Errorf("la nouvelle URL est celle qui sert : %q", got[0].url)
	}
}

func TestSetWebhookAnExistingFileIsReplacedAndItsRightsFixed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "etc", "discord_webhook")
	os.MkdirAll(filepath.Dir(target), 0o755)
	os.WriteFile(target, []byte("https://discord.com/api/webhooks/1/ancienne\n"), 0o644)
	os.Chmod(target, 0o644)
	if _, err := setWebhook(t, target, "discord", goodURL, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	info, _ := os.Stat(target)
	if string(b) != goodURL+"\n" || info.Mode().Perm() != 0o600 {
		t.Errorf("%q %o", b, info.Mode().Perm())
	}
}

func TestSetWebhookInvalidURLsAreRefusedWithoutEchoingThem(t *testing.T) {
	bad := []string{"", "\n", "pas une url", "http://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUv",
		"https://evil.example/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz",
		"https://discord.com/api/webhooks/abc/AbCdEfGhIjKlMnOpQrStUvWxYz",
		"https://discord.com/api/webhooks/123456789012345678/court",
		"https://discord.com.evil.example/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz",
		"https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz espace",
		"https://discord.com/api/webhooks/123456789012345678/" + strings.Repeat("A", 400)}
	for _, u := range bad {
		target := filepath.Join(t.TempDir(), "w")
		out, err := setWebhook(t, target, "discord", u+"\n", false)
		var ce *config.Error
		if err == nil || !errors.As(err, &ce) {
			t.Errorf("%.40q devrait être refusé en erreur de configuration : %v", u, err)
			continue
		}
		if _, statErr := os.Stat(target); statErr == nil {
			t.Errorf("rien ne doit être écrit (%.40q)", u)
		}
		if strings.Contains(out+err.Error(), "AbCdEfGh") {
			t.Errorf("l'URL est répétée : %q %v", out, err)
		}
	}
}

func TestSetWebhookDiscordVariantsAreAccepted(t *testing.T) {
	for _, host := range []string{"discord.com", "discordapp.com", "ptb.discord.com", "canary.discord.com"} {
		u := "https://" + host + "/api/v10/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123"
		if _, err := setWebhook(t, filepath.Join(t.TempDir(), "w"), "discord", u, false); err != nil {
			t.Errorf("%s : %v", host, err)
		}
	}
}

func TestSetWebhookOnlyTheFirstLineIsRead(t *testing.T) {
	target := filepath.Join(t.TempDir(), "w")
	if _, err := setWebhook(t, target, "discord", goodURL+"\nautre chose\n", false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != goodURL+"\n" {
		t.Errorf("%q", b)
	}
}

func TestSetWebhookOtherFormatsAcceptAnyHTTPURL(t *testing.T) {
	if _, err := setWebhook(t, filepath.Join(t.TempDir(), "w"), "ntfy", "https://ntfy.example/mon-sujet", false); err != nil {
		t.Error(err)
	}
	if _, err := setWebhook(t, filepath.Join(t.TempDir(), "w"), "json", "not a url", false); err == nil {
		t.Error("« not a url » devrait être refusé")
	}
	if _, err := setWebhook(t, filepath.Join(t.TempDir(), "w"), "json", "ftp://x/y", false); err == nil {
		t.Error("seul http(s) est accepté")
	}
}

func TestSetWebhookATargetFileIsRequired(t *testing.T) {
	_, err := setWebhook(t, "", "discord", goodURL, false)
	if err == nil || !strings.Contains(err.Error(), "notify_url_file") {
		t.Errorf("%v", err)
	}
}

func TestSetWebhookDryRunWritesNothing(t *testing.T) {
	target := filepath.Join(t.TempDir(), "etc", "w")
	out, err := setWebhook(t, target, "discord", goodURL, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("rien ne doit être écrit")
	}
	if _, statErr := os.Stat(filepath.Dir(target)); statErr == nil {
		t.Error("pas même le dossier")
	}
	if !strings.Contains(out, "[simulation]") || strings.Contains(out, "AbCdEfGh") {
		t.Errorf("sortie : %q", out)
	}
}
