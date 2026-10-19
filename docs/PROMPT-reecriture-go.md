# Mission : réécrire `docker-update` en Go (un seul binaire statique), puis ajouter `init`, `add`, `delete`

Dépôt : `Bloopps/docker-update`. Point de départ : la branche `claude/new-session-4y79zd` (dernier commit : « Scan manuel : check et
run sur tous les services ou une sélection »). Travaille sur la branche que ton environnement t'indique ; à défaut, crée
`claude/go-rewrite` à partir de ce point. **Ne crée aucune pull request** sans demande explicite.

## 1. Pourquoi, et ce qui est décidé

`docker-update` met à jour des images Docker Compose sur l'hôte, avec un délai de sécurité (remplaçant de Watchtower). Il existe
aujourd'hui en Python (`docker-update.py`, 1 730 lignes, bibliothèque standard, Python ≥ 3.11) avec **269 tests**. Le propriétaire
veut le réécrire en Go **maintenant, avant toute nouvelle fonctionnalité**, pour :

- un **binaire unique, statique**, sans Python à installer sur le serveur ;
- des commandes de configuration `init`, `add`, `delete` (phase 7) bâties sur le nouveau code, pas sur l'ancien.

Décisions déjà prises (ne les remets pas en cause ; signale seulement un blocage réel) :

1. **Go ≥ 1.24, bibliothèque standard uniquement.** `go.mod` sans `require`, pas de `go.sum`, pas de `vendor/`. Même règle qu'avant :
   « aucune dépendance tierce ». Conséquences : il n'y a pas de lecteur TOML dans la bibliothèque standard de Go → **écris un
   mini-lecteur TOML strict** (voir §4.2) ; pas de `golang.org/x/term` → saisie masquée avec `syscall` (ioctl `TCGETS`/`TCSETS`, Linux).
2. **On garde le même fonctionnement externe** : l'outil lance toujours la commande `docker` (et `regctl` si configuré) avec **exactement
   les mêmes arguments, dans le même ordre** qu'en Python, sans shell. **Pas de SDK Docker.** Raison : le faux `docker` des tests de bout
   en bout et toute la procédure `docs/AGENT.md` continuent de s'appliquer, et le comportement est comparable ligne à ligne.
3. **Aucun changement de comportement pendant le portage.** Le Python est la **spécification exécutable** ; la réécriture doit faire la
   même chose, message pour message (les tests vérifient des sous-chaînes françaises). Les améliorations viennent après la parité.
4. Linux seulement (`linux/amd64` et `linux/arm64`), `CGO_ENABLED=0`, `-trimpath`, binaire nommé `docker-update`.
5. Fonctionnalités **supprimées volontairement** : retour arrière automatique, crochet `pre_hook`/sauvegarde, signal de vie
   (`heartbeat_url_file`). Leurs clés doivent rester **refusées avec le même message** (« n'existe plus »). Ne les réintroduis pas.

## 2. À lire d'abord (dans cet ordre)

1. `CLAUDE.md` (règles du projet), `README.md` (fonctionnement, notifications, limites), `docs/AGENT.md` (procédure de déploiement).
2. `docker-update.py` en entier : c'est la référence. Repère les sections « Durées », « Configuration », « État », « Notifications »,
   « Rapport », « Cœur » (`Updater`), « Programme principal ».
3. `tests/test_docker_update.py` (2 744 lignes, 269 tests au total avec les e2e), `tests/test_e2e.py`, `tests/e2e/docker` (faux `docker`),
   `examples/*.toml`, `deploy/docker-update.service`, `.github/workflows/ci.yml`.
4. Lance la suite Python pour voir qu'elle passe (`python3 -W error::ResourceWarning -m unittest discover -s tests`). Elle sert de filet
   pendant tout le portage.

## 3. Règles non négociables (reprises de `CLAUDE.md`, valables en Go)

- Aucun shell : `exec.Command(nom, args...)`, jamais `sh -c`. Les commandes sont des listes d'arguments.
- Tout texte venu de l'extérieur (avis et notes de release GitHub, sorties de commandes, noms) est **nettoyé et borné** avant d'être
  journalisé ou notifié (`clean_text`), et `@` est neutralisé dans le rapport.
- **Ne journalise jamais** : l'URL du webhook, le jeton GitHub, la sortie de `docker compose config` (elle contient l'environnement).
  **Piège Go :** les erreurs de `net/http` (`*url.Error`) contiennent l'URL. Ne journalise jamais `err.Error()` d'un appel au webhook :
  journalise le type (« notification non envoyée (…) ») comme le fait le Python.
- Une fonctionnalité qui peut accélérer ou appliquer une mise à jour a **un test qui échoue quand on la casse**. Refais le **test de
  mutation manuel** sur les fonctionnalités critiques (liste en §6.4) : introduis le défaut, vérifie que la suite le détecte.
- Documentation et messages **en français**.
- **N'appelle jamais le vrai Docker** (une CLI `docker` est installée dans l'environnement : ne l'utilise pas), ni l'API GitHub réelle, ni
  Discord. Tests : faux `docker`, `httptest`, serveurs locaux sur `127.0.0.1`.
- Ne passe **aucun service en `mode = "auto"`** dans un exemple ou une commande sans accord explicite de l'utilisateur.

## 4. Conception cible

### 4.1 Découpage (suggestion, à ajuster si tu as mieux)

```
go.mod                                  module github.com/Bloopps/docker-update ; go 1.24
cmd/docker-update/main.go               fin : arguments, code de sortie
internal/textutil                       CleanText, durées, fenêtres horaires, versions, ids courts
internal/tomlmini                       lecteur TOML strict (sous-ensemble) + édition textuelle (phase 7)
internal/config                         Settings, Service, validation, clés supprimées, cohérence des groupes
internal/state                          état JSON (écriture atomique, compatible avec celui du Python)
internal/notify                         Notifier (discord, ntfy, json) + Report (sections, tableaux, budget)
internal/github                         client GET (ETag, jeton, limites), avis, releases, motifs
internal/dockerx                        Runner (liste d'arguments, délai, simulation) + faux monde pour les tests
internal/updater                        evaluate, security, release notes, apply, cleanup, cycle, now, select
internal/cli                            sous-commandes (validate, check, run, now, hold, unhold, status, notify-test, set-webhook, version…)
```

Tout ce qui touche à l'extérieur est **injecté** (comme les paramètres de `Updater.__init__` en Python) : exécuteur de commandes,
horloge, `sleep`, requête HTTP de test de fumée, client GitHub, envoi du webhook, minutes locales. C'est ce qui rend les tests possibles.

### 4.2 Mini-lecteur TOML (sous-ensemble strict)

Support : commentaires `#`, tables `[settings]`, tableaux de tables `[[service]]`, clés simples, chaînes `"…"` (échappements `\\ \" \n \t
\r \uXXXX`) et `'…'`, entiers, booléens, tableaux (sur plusieurs lignes, virgule finale, commentaires) de chaînes ou d'entiers.
**Refuse avec une erreur claire** tout le reste (tables en ligne, flottants, dates, chaînes multilignes, clés pointées) : « TOML non pris
en charge : … ligne N ». Clés dupliquées → erreur. Erreurs avec numéro de ligne. Fuzz-teste-le (`go test -fuzz`, quelques secondes en
CI) : jamais de panique, jamais de boucle infinie. Tous les fichiers de `examples/` doivent se lire. La configuration de l'utilisateur
est un fichier TOML existant : **le lecteur doit accepter tout ce que `tomllib` de Python accepte dans ce sous-ensemble**.

### 4.3 Compatibilité des données

- `state.json` écrit par la version Python doit se **charger tel quel** en Go (et inversement). Schéma : `images{ref:{digest,first_seen,
  notified,failed}}`, `holds[]`, `errors{service:msg}`, `advisories{dépôt:{ok,fetched,etag,items[],err}}`, `alerts{clé:epoch}`,
  `releases{dépôt:{ok,fetched,etag,items[],full,err}}`. Écriture : JSON indenté d'**un** espace, clés triées, atomique (fichier
  temporaire dans le même dossier, `chmod 600`, `rename`). **Piège :** décoder les nombres en entiers (`int64`/`json.Number`), jamais en
  `float64`. Ajoute un fichier de test généré par la version Python (`testdata/`).
- Les fichiers secrets (`notify_url_file`, jeton GitHub) : même format qu'avant (une ligne).
- Codes de sortie identiques : `0` ok, `1` échec ou `UpdateError`, `2` erreur de configuration. Verrou `flock` non bloquant
  (« un autre passage est en cours, rien à faire », code 0). Refus d'une configuration non sûre quand l'outil tourne en root
  (propriétaire root, pas modifiable par le groupe/les autres), sauf `--allow-untrusted-config`.

### 4.4 Pièges de portage Python → Go (liste de contrôle)

- **Ordre déterministe** : l'itération sur une `map` Go est aléatoire. Services, sections du rapport, alertes, clés d'état, tableaux : tout
  doit sortir dans l'ordre du Python (ordre du fichier, clés triées à l'écriture de l'état).
- **Runes, pas octets** : `clean_text(valeur, limite)` tronque en *caractères*. Idem pour la limite Discord (1 900) et les largeurs de
  colonne du tableau (`utf8.RuneCountInString`). `str.isprintable()` ≈ `unicode.IsPrint` ou espace ASCII ; remplace le reste par une espace,
  puis normalise les blancs (`strings.Fields`). `@` → `@` + U+200B après nettoyage.
- **Expressions régulières** : RE2 n'a pas de retour arrière (c'est un avantage : pas de ReDoS) mais `\w` et `\b` sont **ASCII** en Go.
  Garde les motifs de `RELEASE_PATTERNS` à l'identique avec `(?i)` ; vérifie par test les cas limites (`CVE-2026-123` non, `xCVE-…` non,
  `GHSA-` avec l'alphabet `23456789cfghjmpqrvwx`, `auth\w{0,15} bypass`, etc.). Les motifs `release_notes_ignore` de l'utilisateur sont
  compilés en `(?i)` ; une syntaxe non RE2 (lookahead…) doit donner une erreur de configuration claire.
- **HTTP** : la requête de test de fumée ne suit **pas** les redirections (`CheckRedirect` → `http.ErrUseLastResponse`), délai 10 s, code
  `0` si la connexion échoue. GitHub : délai 15 s, en-têtes `Accept: application/vnd.github+json`, `X-GitHub-Api-Version: 2022-11-28`,
  `User-Agent: docker-update/1.0`, `Authorization: Bearer …` si jeton, `If-None-Match` si ETag ; retour `(code, corps, etag)`, **code 0**
  si réseau ou JSON invalide ; limite de lecture **2 Mio** (avis) et **8 Mio** pour le chemin se terminant par `/releases` : lis
  `limite+1` octets et traite un dépassement comme un échec (le Python tronque puis échoue au décodage).
- **Webhook** : Discord = JSON `{"content": …(≤1 900 caractères), "allowed_mentions": {"parse": []}}` ; ntfy = corps texte, en-têtes `Title`
  (ASCII, le reste remplacé par `?`) et `Priority` ; json = `{"title","text","level"}`. Délai 10 s. `json.Encoder.SetEscapeHTML(false)`.
- **Processus** : délai par contexte ; code de retour `124` si délai dépassé, `127` si commande introuvable ; sortie d'octets
  conservée pour hacher le manifeste (digest = SHA-256 des octets exacts). En simulation (`--dry-run`), les commandes « modifiantes » ne
  sont que journalisées (`[simulation] ` + arguments joints avec guillemets de type `shlex.join`).
- **Heure** : fenêtres horaires en heure **locale** (minutes depuis minuit), fenêtre qui traverse minuit ; horloge injectée partout.
- **Mots clés de la configuration** : bools stricts (`"true"` entre guillemets refusé), durées `30m 6h 3d 1w 0`, noms et dépôts validés par
  les mêmes expressions (`NAME_RE`, `REPO_RE`, `GHSA_RE`), clés inconnues refusées avec le même message.

## 5. Plan par phases (un commit + un `git push` par phase, tests au vert)

Avant de coder, écris et commite `docs/PORTAGE.md` (avec une section « Propositions » vide, voir §12) : (a) l'inventaire **test Python → test Go** (tableau, un test Python = une ligne ; rien
n'est abandonné sans justification écrite), (b) la liste des écarts connus, (c) le plan ci-dessous ajusté. Mets-le à jour à chaque phase.

| Phase | Contenu | Critère d'acceptation |
| --- | --- | --- |
| 0 | `go.mod`, squelette, CI Go (voir §7), `docs/PORTAGE.md` | `go vet`, `gofmt -l` vides, CI verte |
| 1 | `textutil` : durées, fenêtres, `CleanText`, versions (`parse_version`, `parse_release_version`, `in_range`), `split_ref`, motifs `scan_notes`, `compact_advisory`, `compact_release` | tests portés de `TestHelpers`, `TestVersions`, `TestReleaseScan` |
| 2 | `tomlmini` + `config` (+ clés supprimées, groupes, `examples/`) | `TestConfig` + fuzz + exemples lus |
| 3 | `state`, `notify` (Notifier, **Report**), `github`, `dockerx` | `TestInfra`, `TestReport`, `TestGithubGet`, `TestSetWebhook` (partie logique), compatibilité `state.json` |
| 4 | `updater` : `evaluate` (dont conteneurs arrêtés), `security_pass`, `release_scan/alerts`, `apply`, `_fail`, `_cleanup`, `cycle`, `now`, `select`, `status_row` | classes `TestCycle`, `TestFailures`, `TestModesAndPolicies`, `TestGroups`, `TestNow`, `TestDryRun`, `TestSecurity*`, `TestRelease*`, `TestCleanup`, `TestStoppedContainers`, `TestAdvisoriesAndReleaseNotesTogether`, `TestReportDelivery`, `TestManualScan` |
| 5 | `cli` + `main` : `validate`, `check [NOM…] [--notify]`, `run [NOM…]`, `now`, `hold`, `unhold`, `status`, `notify-test`, `set-webhook`, `version` | `TestMain` ; **parité e2e** (§6.2) |
| 6 | Bascule : suppression du Python, docs, CI de publication, unité systemd | §8 |
| 7 | `init`, `add`, `delete` (+ `list`) | §9 |

Ne saute pas la phase 5 : la **parité bout en bout** décide si la réécriture est bonne.

## 6. Stratégie de tests

### 6.1 Unitaires Go

Reporte **chaque test Python** (même intention, mêmes messages, mêmes scénarios). Écris l'équivalent Go du « faux monde » (`World` dans
`tests/test_docker_update.py` : registre, images, conteneurs, compose, échecs injectés) derrière l'interface `Runner`. Tables de cas pour
les fonctions pures. `httptest` pour GitHub et le webhook. `go test -race -count=1 ./...` doit passer ; `t.Parallel()` seulement si
aucun état global.

Inventaire à reporter (Python) : `TestHelpers 3, TestConfig 6, TestCycle 7, TestFailures 7, TestModesAndPolicies 12, TestGroups 5,
TestNow 4, TestDryRun 2, TestInfra 7, TestSecurityProperties 3, TestVersions 6, TestSecurity 28, TestGithubGet 3, TestMain 6,
TestReleaseScan 10, TestReleaseNotes 33, TestReleaseBodyLimit 1, TestCleanup 10, TestStoppedContainers 7,
TestAdvisoriesAndReleaseNotesTogether 11, TestReport 9, TestReportDelivery 10, TestSetWebhook 12, TestManualScan 11` (= 213), plus les
e2e : `E2E 8, Advisories 4, ReleaseNotes 5, OneReport 1, SetWebhook 1` (+ classes héritées, soit 269 exécutions au total).

### 6.2 Parité bout en bout (obligatoire)

Rends `tests/test_e2e.py` **indépendant de l'implémentation** : si la variable d'environnement `DU_BIN` est définie, il lance ce binaire
au lieu de `python docker-update.py`. Les 19 tests e2e (56 exécutions avec l'héritage des classes) doivent passer **avec le binaire Go** (et toujours avec le Python tant qu'il existe).
Le faux `docker` (`tests/e2e/docker`, en Python) reste utilisable tel quel pendant la transition ; en phase 6, **porte-le en Go**
(`testdata/fakedocker`, compilé par `TestMain`) pour que `go test ./...` se suffise à lui-même, sans Python.
Ajoute un **test différentiel** : pour chaque scénario e2e, lance le Python et le Go sur des mondes identiques et compare code de sortie,
`stdout` normalisé, `state.json` et messages POST reçus. Toute différence est un défaut du portage ou un écart à documenter.

### 6.3 Tests ajoutés par le portage

Fuzz du mini-lecteur TOML ; chargement d'un `state.json` produit par le Python ; `go vet` ; vérification que l'URL du webhook et le jeton
n'apparaissent dans **aucune** sortie (test sur `stdout`/`stderr`/journal avec un secret reconnaissable) ; test que `docker compose
config` n'est jamais journalisé ; test de non-régression « pas de shell » (aucun appel à `sh`).

### 6.4 Mutation manuelle (obligatoire avant la phase 6)

Sur une **copie** du dépôt, introduis un défaut à la fois et vérifie que `go test ./...` échoue. Au minimum : délai de sécurité ignoré ;
tirage par étiquette au lieu du digest ; fenêtre horaire ignorée ; note de release qui accélère ; pré-version non ignorée ; signal faible
traité comme fort ; dédoublonnage des alertes supprimé ; `@everyone` non neutralisé ; jeton ou URL journalisés ; conteneur arrêté touché ;
retour arrière réintroduit ; nettoyage forcé (`-f`) ou après un échec ; sélection `check NOM` ignorée ; purge de l'état par une
sélection ; permissions du fichier webhook ; hôte webhook non validé ; budget du rapport ignoré. Consigne les résultats dans le README
(comme le Python : « N défauts introduits, N détectés »). Un survivant = un test manquant : écris-le.

## 7. Compilation, CI, publication

- Compilation : `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=<git describe>" -o docker-update ./cmd/docker-update`
  pour `linux/amd64` et `linux/arm64`. Commande `docker-update version` (affiche version, `GOOS/GOARCH`, `go version`).
- CI (`.github/workflows/ci.yml`) : `actions/setup-go` avec version **épinglée**, `gofmt -l .` (échoue s'il imprime quelque chose),
  `go vet ./...`, `go test -race -count=1 ./...`, fuzz court (10 s), compilation des deux cibles, `sha256sum` des binaires,
  dépôt des binaires en artefacts. Pas de publication de release sans demande.
- `.gitignore` : `/docker-update`, `/dist/`. Pas de binaire dans le dépôt.
- Réutilise l'action `actions/checkout` à la version déjà utilisée par le dépôt.

## 8. Bascule (phase 6)

1. Quand la parité e2e et les tests unitaires portés sont verts et la mutation faite : supprime `docker-update.py`, `tests/*.py`,
   `tests/e2e/docker` (si porté en Go) et ce qui n'a plus de sens ; garde `examples/`, `deploy/`, `docs/`.
2. Mets à jour `CLAUDE.md` (« Outil Go (bibliothèque standard seulement, Go ≥ 1.24) », commandes de test : `go test ./...`,
   `go test -run TestX ./internal/updater`, `go vet ./...`), `README.md` (installation = un binaire, plus de Python ; nombre de tests ;
   mutation), `docs/AGENT.md` (prérequis sans Python ; installation `install -m 755 -o root -g root docker-update
   /usr/local/sbin/docker-update` ; l'étape de tests `go test ./...` ; compteur de tests).
3. Unité systemd `deploy/docker-update.service` : vérifie la compatibilité avec un binaire Go (`MemoryDenyWriteExecute=yes`,
   `SystemCallArchitectures=native`, `ProtectSystem=strict`), **supprime** la ligne `ReadWritePaths=` et son commentaire sur la
   sauvegarde de la tirelire (fonctionnalité supprimée). Le service garde `check` par défaut.
4. `.gitignore` : retire `heartbeat_url`.
5. Rédige dans le README la **procédure de migration** : l'état `state.json` et la configuration existants fonctionnent sans changement.

## 9. Nouvelles commandes (phase 7) : `init`, `add`, `delete` (+ `list`)

Principe commun : le fichier TOML reste **la** source de vérité, écrit à la main ou par ces commandes. L'édition est **textuelle et non
destructive** : les commentaires et la mise en forme de l'utilisateur sont conservés. Chaque écriture : (1) affiche un aperçu
(`diff` unifié) et demande confirmation (`--yes` pour passer, `--dry-run` pour ne rien écrire) ; (2) sauvegarde l'ancien fichier en
`updates.toml.bak-AAAAMMJJ-HHMMSS` (mêmes droits) ; (3) écrit de façon atomique avec les mêmes propriétaire et droits que l'original ;
(4) **relit et valide** le résultat avec le même code que `validate` et **annule** si invalide ; (5) prend le verrou `flock`. Toutes les
invites sont en français, lisent l'entrée standard (testable avec un tube) et ont une version non interactive par options. Les
messages d'erreur ne répètent jamais un secret.

### 9.1 Édition textuelle (`internal/tomlmini`)

Découpe le fichier en : préambule, table `[settings]`, blocs `[[service]]`. Un bloc = de sa ligne `[[service]]` jusqu'à la ligne
précédant le prochain en-tête de table ; les **lignes de commentaire collées juste au-dessus** (sans ligne vide) lui appartiennent.
Ajout : le bloc canonique (ordre de clés fixe : `name, compose_dir, service, compose_files, mode, cooldown, group, window, cleanup,
advisories, min_severity, security_cooldown, security_policy, release_notes, release_notes_repos, release_notes_ignore, smoke_url,
smoke_expect, health_timeout`) est inséré avec une ligne vide avant. **Invariant à tester par propriété** : `delete(add(cfg, s), s.name)`
redonne `cfg` **octet pour octet**, et `lire(écrire(x))` redonne `x`. Tests « golden » avec commentaires, lignes vides, fin de fichier sans
saut de ligne, caractères non ASCII, blocs en groupe.

### 9.2 `docker-update init`

Crée une configuration **neuve** (refuse si le fichier existe, sauf `--force` qui sauvegarde d'abord ; pour un fichier existant, indique
`add`). Étapes, en **lecture seule** sur Docker :

1. Découverte : `docker ps` puis `docker inspect` des conteneurs qui tournent → nom, image, étiquettes `com.docker.compose.project`,
   `.service`, `.project.working_dir`, `.project.config_files` (comme l'étape 2 de `docs/AGENT.md`). Ignore les conteneurs hors compose.
2. Contrôle du nom de projet : `docker compose --project-directory D -f … config --format json` doit donner le **même** nom de projet
   que l'étiquette ; sinon le service est écarté avec un avertissement (l'outil ne retrouverait pas le conteneur). **Ne journalise jamais
   la sortie de `config`.** Vérifie que chaque fichier compose existe.
3. Par service : suivre ? (défaut non pour `watchtower`, `docker-socket-proxy`, et pour les **bases de données** reconnues par l'image
   — postgres, mysql, mariadb, mongo, redis, valkey… — avec une explication) ; mode (`notify` par défaut ; `auto` demande de taper une
   confirmation explicite, service par service) ; groupe (propose de regrouper les services d'un même projet qui partagent la même
   image, dans l'ordre du fichier compose) ; `cleanup` ; dépôt GitHub pour `advisories` / `release_notes_repos` (table intégrée des images
   courantes ; pour `ghcr.io/<propriétaire>/<nom>` propose `<propriétaire>/<nom>` « à confirmer » ; sinon laisse vide) ; `release_notes`
   proposé **seulement** si l'image porte une version lisible (`docker image inspect` → `org.opencontainers.image.version`).
4. Webhook : propose de lancer le même parcours que `set-webhook` (saisie masquée).
5. Aperçu complet du fichier, confirmation, écriture, puis `validate` et affichage de la suite (`docker-update check`). `init` ne lance
   **aucune** mise à jour et ne touche à aucun conteneur.
Version non interactive : `init --yes --mode notify [--include nom,nom] [--exclude nom,nom]`.

### 9.3 `docker-update add NOM`

Ajoute **un** service. Sources : un conteneur qui tourne (`--container NOM|ID`, mêmes lectures que `init`) ou des options explicites
(`--compose-dir`, `--service`, `--compose-file` répétable, `--mode`, `--cooldown`, `--window`, `--group`, `--advisories dépôt,…`,
`--release-notes`, `--release-notes-repos`, `--release-notes-ignore`, `--cleanup=false`, `--smoke-url` + `--smoke-expect`). Refuse : nom
déjà pris, nom de groupe égal à un nom de service, réglages incompatibles avec ceux des membres du groupe (reprends alors les réglages
du groupe par défaut). Dans un groupe, l'**ordre du fichier est l'ordre de mise à jour** : insère le nouveau membre après le dernier du
groupe (option `--after SERVICE` ou `--first`), jamais au hasard. Sans option, interactif.

### 9.4 `docker-update delete NOM…` (et `delete --group G`)

Supprime les blocs désignés (et les commentaires collés au-dessus, voir §9.1). Affiche exactement ce qui part et demande confirmation.
Refuse un nom inconnu ; si la suppression laisse un groupe incohérent, l'explique. **Ne touche à aucun conteneur ni image.** Nettoie
aussi l'état : retire le service de `holds` et `errors`, et les `alerts` dont la clé le nomme (`…:NOM:…`) ; les compteurs d'images
(`images`) se purgent au prochain passage complet (`--keep-state` pour ne rien toucher). Le message final rappelle que le conteneur
continue de tourner, simplement hors du suivi.

### 9.5 `docker-update list`

Lecture seule : tableau service / groupe / mode / délai / fenêtre / veille (avis, notes) / pause. Utile pour vérifier `add` et `delete`.

## 10. Définition de « terminé »

- `go build` produit un binaire statique (`file` indique « statically linked ») pour amd64 et arm64.
- `gofmt -l .` vide ; `go vet ./...` propre ; `go test -race -count=1 ./...` vert ; fuzz TOML sans échec ; CI verte.
- Les 19 tests e2e (56 exécutions) passent avec le binaire Go ; le test différentiel Python/Go ne montre aucun écart non documenté (avant suppression du Python).
- Mutation manuelle faite, résultats dans le README, aucun survivant non justifié.
- Un `state.json` et un `updates.toml` existants fonctionnent sans modification.
- `init`, `add`, `delete`, `list` testés (golden + propriété d'aller-retour + scénarios interactifs par tube) et documentés
  (README, `docs/AGENT.md`, l'étape 2 de la procédure de déploiement s'appuie désormais sur `init`).
- Aucun secret dans les journaux, les sorties, les commits. Aucun appel au vrai Docker, à GitHub ou à Discord pendant les tests.
- `docs/PORTAGE.md` à jour : chaque test Python a son équivalent Go ou une justification, et la section « Propositions » est remise au
  propriétaire (§12).

## 11. Conduite

- Un commit par phase, message en français, terminé par les lignes d'attribution demandées par ton environnement. `git push -u origin
  <branche>` après chaque phase (réessaie en cas d'échec réseau : 2 s, 4 s, 8 s, 16 s).
- Après chaque phase : un compte rendu court (ce qui est fait, tests, écarts, prochaine phase). Ne t'arrête que pour une vraie question
  d'architecture ou de comportement que ni ce document, ni le Python, ni le README ne tranchent ; en cas de doute sur le comportement,
  **le Python fait foi**.
- Ne mélange pas portage et amélioration : **n'implémente aucune amélioration pendant les phases 0 à 6**.

## 12. Propositions d'amélioration (demandées par le propriétaire)

Tu lis tout le code et tous les tests : tu es bien placé pour voir ce qui peut être mieux. **Le propriétaire veut que tu les lui
proposes**, y compris sur ce qui existe déjà (code, fonctionnalités, messages, documentation, procédure de déploiement, tests).

- Tiens à jour, dans `docs/PORTAGE.md`, une section **« Propositions »**. Une entrée = *quoi* (une phrase), *pourquoi* (le problème
  réel, avec un exemple ou une référence `fichier:ligne`), *effet sur le comportement* (aucun / visible : lequel), *risque*, *effort*
  (petit / moyen / gros), *ta recommandation* (faire / ne pas faire / à discuter). Pas de liste de souhaits vagues.
- Sont à signaler : un **défaut ou une incohérence** du Python (écris-le dès que tu le vois, avec un test qui le montre, et dis si tu l'as
  reproduit ou corrigé en Go), un cas non couvert par les tests, une fonctionnalité fragile, un message peu clair, une dépendance à
  une sortie de `docker` mal vérifiée, un point de sécurité, une simplification possible, une option de configuration inutile ou
  manquante, une possibilité que Go offre (parallélisme des lectures du registre, lecture directe du registre sans `buildx`, interface
  Docker directe…).
- **Ne les applique pas.** Présente-les au propriétaire dans le compte rendu de chaque phase (les nouvelles seulement, en quelques
  lignes) et dans un récapitulatif **à la fin de la phase 6**, avant les phases `init` / `add` / `delete` : il choisira lesquelles
  entrent dans la phase 7 ou plus tard. Seule exception : un défaut qui rend la **parité impossible** ou qui est dangereux
  (secret journalisé, commande non bornée) : corrige-le dans le Python et le Go avec un test, et signale-le tout de suite.
- Pendant la phase 7, applique la même règle aux trois nouvelles commandes : propose ce qui manque (par exemple `rename`, `edit`,
  export de la configuration, vérification des dépôts GitHub), sans l'ajouter de toi-même.
