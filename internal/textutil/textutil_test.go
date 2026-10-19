package textutil

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

const day = 86400

// ── TestHelpers ─────────────────────────────────────────────────────────────

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "30m": 1800, "6h": 21600, "3d": 3 * day, "1w": 7 * day, " 3d ": 3 * day} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %d, %v ; attendu %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "3", "d", "3 jours", "-1d", "1.5d", "3D", "99999999999999999999d", "9223372036854775807w"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) devrait échouer", bad)
		} else if !strings.Contains(err.Error(), "durée invalide") || !strings.Contains(err.Error(), "exemples : 30m, 6h, 3d, 1w, 0") {
			t.Errorf("message inattendu : %v", err)
		}
	}
	if _, err := ParseDuration("3"); err == nil || err.Error() != "durée invalide : '3' (exemples : 30m, 6h, 3d, 1w, 0)" {
		t.Errorf("message : %v", err)
	}
}

func TestFmtDuration(t *testing.T) {
	for in, want := range map[int64]string{0: "0 s", 59: "59 s", 60: "1 min", 3599: "59 min", 3600: "1 h", 6 * 3600: "6 h", 86400: "1 j", 3 * 86400: "3 j"} {
		if got := FmtDuration(in); got != want {
			t.Errorf("FmtDuration(%d) = %q ; attendu %q", in, got, want)
		}
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct{ in, repo, tag, digest string }{
		{"traefik:v3.6", "traefik", "v3.6", ""},
		{"traefik", "traefik", "latest", ""},
		{"ghcr.io/org/app:main", "ghcr.io/org/app", "main", ""},
		{"localhost:5000/app", "localhost:5000/app", "latest", ""},
		{"localhost:5000/app:1.2", "localhost:5000/app", "1.2", ""},
		{"app:1@sha256:abc", "app", "1", "sha256:abc"},
	}
	for _, c := range cases {
		repo, tag, digest := SplitRef(c.in)
		if repo != c.repo || tag != c.tag || digest != c.digest {
			t.Errorf("SplitRef(%q) = %q %q %q ; attendu %q %q %q", c.in, repo, tag, digest, c.repo, c.tag, c.digest)
		}
	}
}

func TestWindows(t *testing.T) {
	w, err := ParseWindow("")
	if err != nil || w.Set {
		t.Fatalf("fenêtre vide : %+v, %v", w, err)
	}
	if !InWindow(w, 999) {
		t.Error("sans fenêtre : toujours dedans")
	}
	w, err = ParseWindow("02:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	for minutes, want := range map[int]bool{120: true, 299: true, 300: false, 60: false} {
		if got := InWindow(w, minutes); got != want {
			t.Errorf("02:00-05:00 à %d min : %v", minutes, got)
		}
	}
	night, _ := ParseWindow("22:00-04:00")
	for minutes, want := range map[int]bool{23 * 60: true, 3 * 60: true, 12 * 60: false, 4 * 60: false, 22 * 60: true} {
		if got := InWindow(night, minutes); got != want {
			t.Errorf("22:00-04:00 à %d min : %v", minutes, got)
		}
	}
	for _, bad := range []string{"2-5", "25:00-26:00", "02:00-02:00", "02:61-03:00", "nuit"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) devrait échouer", bad)
		}
	}
	if _, err := ParseWindow("02:00-02:00"); err == nil || err.Error() != "fenêtre vide : '02:00-02:00'" {
		t.Errorf("message : %v", err)
	}
}

// ── TestVersions ────────────────────────────────────────────────────────────

func TestParseVersion(t *testing.T) {
	good := map[string]Version{"3.6.3": {3, 6, 3, 1}, "v3.6.3": {3, 6, 3, 1}, "3.6.3-rc1": {3, 6, 3, 0}, "3.6.3+build5": {3, 6, 3, 1}}
	for in, want := range good {
		if got, ok := ParseVersion(in); !ok || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v ; attendu %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "latest", "3.6", "3", "v3.x.1", "3.6.3.1", "1.0.0 beta", "99999999999999999999.0.0"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) devrait être illisible", bad)
		}
	}
}

func TestRanges(t *testing.T) {
	cases := []struct {
		v, r string
		want bool
	}{
		{"3.6.2", "< 3.6.3", true}, {"3.6.3", "< 3.6.3", false}, {"3.6.4", "< 3.6.3", false},
		{"3.0.0", ">= 3.0.0, < 3.6.3", true}, {"2.11.9", ">= 3.0.0, < 3.6.3", false},
		{"3.6.3", "<= 3.6.3", true}, {"3.6.3", "> 3.6.3", false}, {"3.6.3", "= 3.6.3", true},
		{"3.6.3", "3.6.3", true}, {"v3.6.2", "< 3.6.3", true}, {"3.6.3-rc1", "< 3.6.3", true},
		{"1.9.9", "< 1.10.0", true}, {"1.10.0", "< 1.10", false}, {"2.0.0", "< 10.0.0", true},
	}
	for _, c := range cases {
		got, err := InRange(c.v, c.r)
		if err != nil || got != c.want {
			t.Errorf("%s dans %s : %v, %v ; attendu %v", c.v, c.r, got, err, c.want)
		}
	}
}

func TestUnreadableNeverGuesses(t *testing.T) {
	for _, c := range [][2]string{{"latest", "< 3.6.3"}, {"3.6.2", ""}, {"3.6.2", "autour de 3.6"}, {"3.6.2", "<< 3"}, {"3.6.2", ">= x.y"}} {
		if _, err := InRange(c[0], c[1]); err == nil {
			t.Errorf("InRange(%q, %q) devrait échouer", c[0], c[1])
		}
	}
	// Comme en Python : la première contrainte qui échoue conclut, les suivantes ne sont pas lues.
	if got, err := InRange("3.6.3", "< 3.0.0, n'importe quoi"); err != nil || got {
		t.Errorf("évaluation paresseuse : %v, %v", got, err)
	}
}

func TestSevRank(t *testing.T) {
	for in, want := range map[string]int{"low": 0, "medium": 1, "HIGH": 2, "Critical": 3, "": 1, "inconnue": 1} {
		if got := SevRank(in); got != want {
			t.Errorf("SevRank(%q) = %d ; attendu %d", in, got, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText("a\n\n b\x00c\t d", 200); got != "a b c d" {
		t.Errorf("got %q", got)
	}
	if got := len([]rune(CleanText(strings.Repeat("x", 999), 200))); got != 200 {
		t.Errorf("limite : %d", got)
	}
	if got := CleanValue(nil, 200); got != "" {
		t.Errorf("nil : %q", got)
	}
	// Bornes en caractères, pas en octets.
	if got := CleanText(strings.Repeat("é", 300), 200); len([]rune(got)) != 200 {
		t.Errorf("runes : %d", len([]rune(got)))
	}
	// Espaces insécables, séparateurs de ligne, caractères de formatage : remplacés par une espace.
	if got := CleanText("a\u00a0b\u2028c\u200bd\u202ee", 200); got != "a b c d e" {
		t.Errorf("got %q", got)
	}
	if got := CleanText("naïve — 日本語 ✅", 200); got != "naïve — 日本語 ✅" {
		t.Errorf("le texte imprimable est conservé : %q", got)
	}
	if got := CleanText("\xff\xfe ok", 200); got != "�� ok" {
		t.Errorf("octets invalides : %q", got)
	}
}

func TestShortID(t *testing.T) {
	if got := ShortID("sha256:0123456789abcdef0123"); got != "0123456789ab" {
		t.Errorf("got %q", got)
	}
	if got := ShortID("id-court"); got != "id-court" {
		t.Errorf("got %q", got)
	}
	if got := len(ShortID(strings.Repeat("z", 100))); got != 40 {
		t.Errorf("borné à 40 : %d", got)
	}
}

func TestRepr(t *testing.T) {
	cases := map[string]string{"abc": "'abc'", "l'eau": `"l'eau"`, `a"b'c`: `'a"b\'c'`, "a\nb": `'a\nb'`, "": "''", "é": "'é'", "\x00": `'\x00'`, `\`: `'\\'`}
	for in, want := range cases {
		if got := Repr(in); got != want {
			t.Errorf("Repr(%q) = %s ; attendu %s", in, got, want)
		}
	}
}

// advisory construit un avis GitHub (JSON décodé), comme la fonction de test du Python.
func advisory(now int64, over map[string]any) map[string]any {
	a := map[string]any{
		"ghsa_id": "GHSA-2222-3333-4444", "cve_id": "CVE-2026-12345", "severity": "high",
		"published_at": time.Unix(now-3600, 0).UTC().Format(time.RFC3339), "summary": "Contournement d'authentification",
		"withdrawn_at": nil,
		"vulnerabilities": []any{map[string]any{"package": map[string]any{"ecosystem": "go", "name": "x"},
			"vulnerable_version_range": "< 3.6.3", "patched_versions": "3.6.3"}},
	}
	for k, v := range over {
		a[k] = v
	}
	return a
}

func TestCompactAdvisory(t *testing.T) {
	const now = 1_000_000
	a, ok := CompactAdvisory(advisory(now, nil))
	if !ok || a.ID != "GHSA-2222-3333-4444" || a.CVE != "CVE-2026-12345" || a.Sev != "high" || !slices.Equal(a.Ranges, []string{"< 3.6.3"}) {
		t.Fatalf("avis : %+v, %v", a, ok)
	}
	if a.Pub != now-3600 || a.Withdrawn {
		t.Errorf("pub/withdrawn : %+v", a)
	}
	if w, _ := CompactAdvisory(advisory(now, map[string]any{"withdrawn_at": "2026-01-01T00:00:00Z"})); !w.Withdrawn {
		t.Error("un avis retiré doit l'indiquer")
	}
	if c, _ := CompactAdvisory(advisory(now, map[string]any{"cve_id": "pas-un-cve"})); c.CVE != "" {
		t.Errorf("CVE invalide : %q", c.CVE)
	}
	if c, _ := CompactAdvisory(advisory(now, map[string]any{"published_at": "2026-05-01T10:00:00Z"})); c.Pub != 1777629600 {
		t.Errorf("date en Z : %d", c.Pub)
	}
	for _, bad := range []any{nil, 3.0, map[string]any{}, map[string]any{"ghsa_id": "GHSA-aaaa-bbbb-cccc"},
		advisory(now, map[string]any{"published_at": "hier"}), advisory(now, map[string]any{"ghsa_id": "n'importe quoi"}),
		advisory(now, map[string]any{"published_at": nil})} {
		if _, ok := CompactAdvisory(bad); ok {
			t.Errorf("devrait être refusé : %v", bad)
		}
	}
	weird, _ := CompactAdvisory(advisory(now, map[string]any{"summary": "@everyone\n```rm -rf```\x00" + strings.Repeat("z", 500)}))
	if strings.Contains(weird.Summary, "\n") || len([]rune(weird.Summary)) > 200 {
		t.Errorf("résumé non nettoyé : %q", weird.Summary)
	}
	// Plages absentes ou invalides : ignorées, la liste reste vide (et non nulle : « [] » dans l'état).
	e, _ := CompactAdvisory(advisory(now, map[string]any{"vulnerabilities": []any{map[string]any{"vulnerable_version_range": "  "}, "x", nil}}))
	if e.Ranges == nil || len(e.Ranges) != 0 {
		t.Errorf("plages : %#v", e.Ranges)
	}
}

// ── TestReleaseScan ─────────────────────────────────────────────────────────

func kinds(text string, weak bool) []string {
	set := map[string]bool{}
	for _, h := range ScanNotes(text) {
		if h.W == weak {
			set[h.K] = true
		}
	}
	out := []string{}
	for k := range set {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestParseReleaseVersion(t *testing.T) {
	good := map[string]Version{"v2.45.1": {2, 45, 1, 1}, "24.04": {24, 4, 0, 1}, "2026.7.8": {2026, 7, 8, 1}, "3.7.0-rc1": {3, 7, 0, 0}, "1.2.3+build": {1, 2, 3, 1}}
	for in, want := range good {
		if got, ok := ParseReleaseVersion(in); !ok || got != want {
			t.Errorf("ParseReleaseVersion(%q) = %v, %v ; attendu %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "latest", "main", "18-vectorchord0.5.3", "release-1.2.3", "1", "1.2.3.4", "v1.x", "1.2 beta", "1234567.1.1"} {
		if _, ok := ParseReleaseVersion(bad); ok {
			t.Errorf("ParseReleaseVersion(%q) devrait être illisible", bad)
		}
	}
	rc, _ := ParseReleaseVersion("3.7.0-rc1")
	final, _ := ParseReleaseVersion("3.7.0")
	if rc.Compare(final) >= 0 {
		t.Error("une pré-version est plus petite")
	}
	a, _ := ParseReleaseVersion("v24.4.0")
	b, _ := ParseReleaseVersion("v24.22.0")
	if a.Compare(b) >= 0 {
		t.Error("comparaison numérique et non textuelle")
	}
}

func TestStrongPatterns(t *testing.T) {
	cases := map[string][]string{
		"Fixes CVE-2026-12345 and cve-2025-1234567":                        {"CVE"},
		"see GHSA-qxcr-4rv3-v23r":                                          {"GHSA"},
		"This is a Security Fix release":                                   {"correctif de sécurité"},
		"several security fixes, a security update, the security advisory": {"correctif de sécurité"},
		"a vulnerability, several vulnerabilities, vulnerable to":          {"vulnérabilité"},
		"remote code execution (RCE), xss, SSRF and CSRF":                  {"RCE/XSS/SSRF/CSRF"},
		"SQL injection in search":                                          {"injection SQL"},
		"privilege escalation":                                             {"élévation de privilèges"},
		"authentication bypass and auth bypass and authorization bypass":   {"contournement d'authentification"},
		"path traversal when extracting":                                   {"path traversal"},
	}
	for text, want := range cases {
		if got := kinds(text, false); !slices.Equal(got, want) {
			t.Errorf("%q : %v ; attendu %v", text, got, want)
		}
	}
}

func TestNearMissesDoNotMatch(t *testing.T) {
	for _, text := range []string{"CVE-2026-123", "CVE-26-12345", "GHSA-aaaa-bbbb-cccc-dddd", "GHSA-qxcr-4rv3", "GHSA-ILIL-1111-OOOO",
		"securityfix", "insecurity fixes", "the security of fixes", "ARCEUS", "XSSX", "ssrfs", "preSQL injection",
		"xCVE-2026-12345", "CVE-2026-12345x", "sql  injected", "privileged escalation", "authbypass", "unauth_bypass",
		"path-traversals-of", "traversal path"} {
		// « sql  injected » et « path-traversals-of » : à comparer avec le comportement du Python (même liste).
		if got := kinds(text, false); len(got) != 0 {
			t.Errorf("%q ne devrait rien trouver : %v", text, got)
		}
	}
}

func TestWordBoundariesAreUnicode(t *testing.T) {
	// En Python, \b suit la notion Unicode de « caractère de mot » : une lettre accentuée colle au motif.
	for _, text := range []string{"éCVE-2026-12345", "CVE-2026-12345é", "ñsecurity fix", "security fixé", "éSSRF", "日本security fix"} {
		if got := kinds(text, false); len(got) != 0 {
			t.Errorf("%q ne devrait rien trouver : %v", text, got)
		}
	}
	if got := kinds("(CVE-2026-12345)", false); !slices.Equal(got, []string{"CVE"}) {
		t.Errorf("ponctuation : %v", got)
	}
	if got := kinds("été: security fix, éé", false); !slices.Equal(got, []string{"correctif de sécurité"}) {
		t.Errorf("espace + ponctuation : %v", got)
	}
	// Après un rejet, la recherche continue : le second CVE, bien isolé, est trouvé.
	hits := ScanNotes("xCVE-2026-1111 puis CVE-2026-2222")
	if len(hits) != 1 || hits[0].T != "CVE-2026-2222" {
		t.Errorf("hits : %+v", hits)
	}
}

func TestSecurityAloneIsWeak(t *testing.T) {
	if got := kinds("Improve security of the login page", true); !slices.Equal(got, []string{"security"}) {
		t.Errorf("faible : %v", got)
	}
	if got := kinds("Improve security of the login page", false); len(got) != 0 {
		t.Errorf("fort : %v", got)
	}
	if got := kinds("x-security-y", true); !slices.Equal(got, []string{"security"}) {
		t.Errorf("tirets : frontière de mot : %v", got)
	}
	for _, text := range []string{"insecurity", "Securityx", "mysecurity", "security_level"} {
		if got := kinds(text, true); len(got) != 0 {
			t.Errorf("%q : %v", text, got)
		}
	}
	if got := kinds("a security fix", false); !slices.Equal(got, []string{"correctif de sécurité"}) {
		t.Errorf("got %v", got)
	}
}

func TestHitsAreDeduplicatedAndBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(strings.Repeat("CVE-2026-1111 ", 500))
	for i := 2000; i < 2100; i++ {
		fmt.Fprintf(&sb, " CVE-2026-%04d", i)
	}
	var strong, weak []Hit
	for _, h := range ScanNotes(sb.String()) {
		if h.W {
			weak = append(weak, h)
		} else {
			strong = append(strong, h)
		}
	}
	if len(strong) != MaxStrongHits {
		t.Errorf("fortes : %d ; attendu %d", len(strong), MaxStrongHits)
	}
	seen := map[string]bool{}
	for _, h := range strong {
		if seen[h.T] {
			t.Errorf("doublon %q", h.T)
		}
		seen[h.T] = true
	}
	if len(weak) > MaxWeakHits {
		t.Errorf("faibles : %d", len(weak))
	}
	many := ScanNotes(strings.Repeat("a security note. ", 50))
	if len(many) != MaxWeakHits {
		t.Errorf("signaux faibles : %d ; attendu %d", len(many), MaxWeakHits)
	}
	for _, h := range ScanNotes(strings.Repeat("x", 500) + " vulnerable \n\x00 @everyone " + strings.Repeat("y", 500)) {
		if n := len([]rune(h.X)); n > 160 {
			t.Errorf("extrait de %d caractères", n)
		}
		if n := len([]rune(h.T)); n > 60 {
			t.Errorf("texte de %d caractères", n)
		}
		if strings.Contains(h.X, "\n") {
			t.Errorf("saut de ligne dans l'extrait : %q", h.X)
		}
	}
}

func TestOnlyTheHeadOfAHugeNoteIsRead(t *testing.T) {
	text := strings.Repeat("a ", MaxNotes/2) + " CVE-2026-9999"
	if got := ScanNotes(text); len(got) != 0 {
		t.Errorf("la fin de la note ne doit pas être lue : %+v", got)
	}
	// Bornes en caractères : 65 536 « é » (2 octets chacun) puis un CVE : hors limite.
	if got := ScanNotes(strings.Repeat("é", MaxNotes) + " CVE-2026-9999"); len(got) != 0 {
		t.Errorf("limite en caractères : %+v", got)
	}
	if got := ScanNotes(strings.Repeat("é", MaxNotes-20) + " CVE-2026-9999"); len(got) != 1 {
		t.Errorf("dans la limite : %+v", got)
	}
	start := time.Now()
	ScanNotes("vulnerab" + strings.Repeat("a", 70000) + " auth" + strings.Repeat("x", 70000) + strings.Repeat(" ", 70000) + "security fix")
	ScanNotes(strings.Repeat("auth ", 20000) + strings.Repeat("path ", 20000))
	ScanNotes(strings.Repeat("xCVE-2026-1111 ", 5000))
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("motifs bornés : %v", d)
	}
	if got := ScanNotes(""); len(got) != 0 {
		t.Errorf("vide : %+v", got)
	}
}

func release(now int64, tag, body string, over map[string]any) map[string]any {
	r := map[string]any{
		"tag_name": tag, "name": tag, "body": body, "prerelease": false, "draft": false,
		"published_at": time.Unix(now-3600, 0).UTC().Format(time.RFC3339), "html_url": "https://evil.example/phish",
	}
	for k, v := range over {
		r[k] = v
	}
	return r
}

func TestCompactRelease(t *testing.T) {
	const now = 1_000_000
	r, ok := CompactRelease(release(now, "v2.45.1", "Fixes CVE-2026-12345", nil))
	if !ok || r.Tag != "v2.45.1" || r.Draft || r.Pre {
		t.Fatalf("release : %+v, %v", r, ok)
	}
	if r.Pub <= 0 || len(r.Hits) == 0 || r.Hits[0].T != "CVE-2026-12345" {
		t.Errorf("pub/hits : %+v", r)
	}
	if p, _ := CompactRelease(release(now, "v1.0.0", "", map[string]any{"prerelease": true})); !p.Pre {
		t.Error("préversion")
	}
	d, _ := CompactRelease(release(now, "v1.0.0", "", map[string]any{"draft": true, "published_at": nil}))
	if !d.Draft || d.Pub != 0 {
		t.Errorf("un brouillon n'a pas de date de publication : %+v", d)
	}
	// Sans date de publication, la date de création sert.
	c, _ := CompactRelease(release(now, "v1.0.0", "", map[string]any{"published_at": nil, "created_at": "2026-05-01T10:00:00Z"}))
	if c.Pub != 1777629600 {
		t.Errorf("created_at : %d", c.Pub)
	}
	for _, bad := range []any{nil, 3.0, []any{}, map[string]any{}, map[string]any{"tag_name": 5.0},
		release(now, "", "", nil), release(now, "a/b", "", nil), release(now, "../x", "", nil), release(now, "v1 0", "", nil),
		release(now, strings.Repeat("x", 200), "", nil), release(now, "-rf", "", nil), release(now, "v1\n2", "", nil)} {
		if _, ok := CompactRelease(bad); ok {
			t.Errorf("devrait être refusé : %v", bad)
		}
	}
	e, ok := CompactRelease(map[string]any{"tag_name": "v1.0.0", "body": nil})
	if !ok || e.Hits == nil || len(e.Hits) != 0 {
		t.Errorf("corps absent : %#v", e)
	}
}
