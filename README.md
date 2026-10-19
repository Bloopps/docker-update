# docker-update

Mises à jour automatiques d'images **Docker Compose**, sans conteneur qui détienne le droit de créer d'autres conteneurs.

Un outil en Go (bibliothèque standard seulement), livré en **un seul binaire statique**, qui tourne **sur l'hôte**, lancé par un minuteur `systemd`.
Pensé comme une alternative plus sûre à Watchtower pour un serveur personnel.

## Installation

Le binaire Go est statique (aucune dépendance, ni Python ni bibliothèque système) pour Linux **amd64** et **arm64**.
Il lui faut seulement `docker` (avec le plugin `compose`) sur l'hôte, et `regctl` si `registry_tool = "regctl"`.
Aucune version publiée n'existe pour l'instant : on le compile, ou on prend l'artefact `docker-update-linux` de la CI
(`dist/docker-update-linux-amd64` ou `-arm64`, avec `SHA256SUMS`).

```sh
# Compilation (Go 1.27 ou plus récent)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe --tags --always --dirty)" \
  -o docker-update ./cmd/docker-update
./docker-update version

# Installation (root)
sudo install -d -m 755 -o root -g root /etc/docker-update
sudo install -m 755 -o root -g root docker-update /usr/local/sbin/docker-update
sudo install -m 644 -o root -g root updates.toml /etc/docker-update/updates.toml     # partir de examples/ (voir la note sur `init`)
sudo install -m 644 -o root -g root deploy/docker-update.service deploy/docker-update.timer /etc/systemd/system/
sudo install -d -m 700 -o root -g root /var/lib/docker-update /var/lib/docker-update/docker

# Webhook (saisie masquée, jamais en argument) puis vérification, sans rien appliquer
sudo docker-update --config /etc/docker-update/updates.toml set-webhook     # écrit notify_url_file
sudo docker-update --config /etc/docker-update/updates.toml validate
sudo docker-update --config /etc/docker-update/updates.toml --dry-run check
sudo systemctl daemon-reload && sudo systemctl enable --now docker-update.timer
```

**`docker-update init`** fait le reste. Il contrôle d'abord toute l'installation, en lecture seule : Docker et `compose`, `regctl` ou
`buildx`, binaire, dossiers et droits (configuration, état, verrou), fichiers du webhook et du jeton, unités systemd et minuteur (avec leur
mode `check` ou `run`), accès au registre et à GitHub. Chaque point s'affiche ✓, ✗ (bloquant : `init` s'arrête) ou ⚠ (à corriger, n'empêche
pas) avec la commande qui le corrige ; `init --check` rejoue ces contrôles seuls, à tout moment (code 0 si tout est ✓, 1 sinon).
Ensuite il relève les conteneurs Compose qui tournent (lecture seule, jamais leur environnement), vérifie que l'outil retrouvera chacun
(même nom de projet que `docker compose config`, même conteneur pour `compose ps`), pose les questions service par service (suivre ? mode,
groupe, nettoyage, dépôt GitHub des avis, notes de release), affiche le fichier complet, l'écrit après confirmation (droits 644), puis propose
`set-webhook`. Il ne pose pas lui-même le binaire ni les unités : il dit ce qui manque. **Il ne lance aucune mise à jour et ne touche à aucun
conteneur.** Par défaut le mode est `notify` ; `auto` demande de taper `auto` pour chaque service ou groupe. Écartés par défaut :
Watchtower, le proxy du socket Docker et les bases de données (postgres, mysql, mariadb, mongo, redis, valkey, memcached), avec la raison.

Version non interactive : `init --yes [--mode notify|manual|auto] [--include nom,…] [--exclude nom,…]` (pas de groupes ; seuls les dépôts
d'avis certains de la table intégrée sont repris). Un fichier existant n'est jamais écrasé : `init --force` le remplace après sauvegarde.
`init --skip-checks` ne passe que les contrôles non bloquants.

L'unité fournie lance `check` (observation) : elle n'applique rien tant qu'on n'a pas validé le résultat et basculé sur
`run`. La procédure complète, avec les critères d'acceptation de chaque étape et le retour arrière, est dans
[`docs/AGENT.md`](docs/AGENT.md).

## Ce qu'il fait

- **Délai de sécurité mesuré par l'outil.** Une image n'est appliquée que si son *digest* est resté le même pendant
  `cooldown` (3 jours par défaut), compté depuis la première fois que l'outil l'a **vu**. Une date écrite dans l'image,
  qu'un éditeur peut fausser, n'entre pas en jeu. Si l'étiquette bouge avant la fin, le compteur repart.
- **Veille de sécurité.** Il lit les avis de sécurité publiés par les dépôts GitHub des projets suivis. Si un avis
  touche la version **en cours** et que l'image candidate (version lue dans ses étiquettes) n'est **plus** touchée,
  le délai est ramené à `security_cooldown` (6 h par défaut). Tout cas ambigu devient une **alerte**, jamais une accélération.
- **Veille des notes de release** (optionnelle, par service). Beaucoup de projets corrigent des failles sans publier
  d'avis. L'outil lit les notes des releases plus récentes que la version en cours et **alerte** s'il y trouve un motif
  de sécurité. Jamais d'accélération : voir ci-dessous.
- **Application exacte.** Il tire le digest évalué (pas « l'étiquette », qui a pu bouger), retague localement, puis
  recrée le service avec `docker compose up -d --no-deps --pull never --wait`.
- **Pas de retour arrière automatique.** Si le service ne repart pas (sonde de santé, état après démarrage, test de
  fumée HTTP), l'outil **alerte et s'arrête** : une base ou une application a pu migrer, remettre l'ancienne image à
  l'aveugle est pire. L'alerte nomme l'image précédente, qui reste sur le disque, et la commande pour y revenir à la
  main. Le digest en échec n'est pas retenté ; un digest plus récent l'est. Les clés `rollback` et `pre_hook` sont refusées.
- **Nettoyage.** Une fois la mise à jour **réussie**, l'image précédente est supprimée (`docker image rm`, comme le
  nettoyage de Watchtower). Jamais forcé : docker refuse si un conteneur utilise encore l'image ou si elle porte un
  autre nom, et ce n'est alors qu'une ligne de journal. Rien n'est supprimé après un échec (l'image précédente reste
  disponible pour revenir à la main). Réglage `cleanup` (défaut `true`), global ou par service.
- **Conteneurs arrêtés laissés tels quels.** Un service dont le conteneur est arrêté (`exited`, `created`, `paused`, `dead`…)
  n'est touché en rien : ni registre, ni mise à jour, ni démarrage, ni veille de sécurité, ni nettoyage, ni compteur de
  délai. Seuls `running` et `restarting` sont suivis. Démarrez-le vous-même ; il est repris au passage suivant.
- **Fenêtre horaire** (`02:00-05:00`) : on observe tout le temps, on n'applique que dans la fenêtre. Un avis *critique* l'ignore.
- **Groupes** : plusieurs services d'une même image mis à jour ensemble, dans l'ordre du fichier.
- **Modes par service** : `auto` (applique), `notify` (prévient, vous appliquez), `manual` (ignore).
- **Un seul rapport par passage** sur Discord (ntfy ou JSON possibles) : sections par importance, tableau des versions prêtes,
  silence s'il n'y a rien de nouveau. Sans mention possible, sans doublons, sans jamais journaliser l'URL. Voir *Notifications*.
- **Pause** (`hold`), **voie rapide** (`now`), **simulation** (`--dry-run`), verrou contre deux passages simultanés.

## Sous-commandes

| Commande | Effet |
| --- | --- |
| `validate` | vérifie configuration, outils et chemins ; ne touche à rien |
| `check [NOM…] [--notify]` | **scan manuel** de tous les services, ou de ceux désignés (nom de service ou de groupe) : registre, état, tests de fumée, version détectée ; n'applique **rien**, n'envoie **rien**, ne télécharge **rien**. Avec `--notify`, le tableau d'état de la sélection part sur Discord (📋, un message) |
| `run [NOM…]` | observe, applique ce qui est prêt en mode `auto`, notifie ; tous les services, ou la sélection |
| `now NOM… --yes` | applique tout de suite (sans délai ni fenêtre) ; sans `--yes`, simule |
| `hold NOM…` / `unhold NOM…` | met en pause / reprend |
| `status` | état mémorisé |
| `set-webhook` | enregistre l'URL du webhook Discord (saisie masquée ou entrée standard, fichier `600`, jamais affichée) |
| `notify-test` | envoie un message de test (vérifie le webhook) |
| `init [--yes] [--check] [--force]` | vérifie l'installation, relève les conteneurs Compose qui tournent et crée la configuration (voir Installation) |
| `list` | tableau des services configurés : groupe, mode, délai, fenêtre, veille (avis, notes), pause ; lecture seule |
| `add NOM --compose-dir D --service S [options]` | ajoute **un** service au fichier de configuration (voir ci-dessous) |
| `delete NOM… [--group G] [--keep-state]` | retire des services du fichier (voir ci-dessous) |

### Modifier la configuration : `add` et `delete`

Le fichier TOML reste la source de vérité : ces commandes l'éditent **par son texte**, sans toucher à vos commentaires ni à votre
mise en forme. À chaque écriture : aperçu (diff), confirmation (`--yes` pour passer ; `--dry-run` pour ne rien écrire), sauvegarde de
l'ancien fichier en `updates.toml.bak-AAAAMMJJ-HHMMSS` (mêmes droits), écriture atomique avec le même propriétaire et les mêmes droits,
relecture et validation du résultat (l'ancien fichier est remis en place si elle échoue), sous le verrou des passages.

- `add` : `--container NOM|ID` reprend dossier, service et fichiers compose d'un conteneur qui tourne (après la même vérification que `init`) ; options `--compose-file` (répétable), `--mode` (défaut **`notify`**, jamais `auto` sans que vous le disiez), `--cooldown`, `--window`,
  `--group`, `--advisories dépôt,…`, `--release-notes`, `--release-notes-repos`, `--release-notes-ignore`, `--cleanup=false`, `--smoke-url`,
  `--smoke-expect`, `--min-severity`, `--security-cooldown`, `--security-policy`, `--health-timeout`. Ce qui manque (dossier compose, service)
  est demandé sur l'entrée standard. Le nouveau bloc est inséré à la fin, ou après le dernier membre de son groupe (qui lui donne ses
  réglages communs), ou là où disent `--after SERVICE` / `--first` : dans un groupe, l'ordre du fichier est l'ordre de mise à jour.
  Refus : nom déjà pris, nom de groupe égal à un nom de service, réglages incompatibles avec ceux du groupe.
- `delete` retire les blocs désignés avec les commentaires collés au-dessus, et nettoie l'état (pauses, erreurs, alertes de ces services ;
  `--keep-state` pour ne rien toucher). **Aucun conteneur ni image n'est touché** : le conteneur continue de tourner, hors du suivi.
- Les fichiers aux fins de ligne Windows (`\r\n`) sont refusés par ces deux commandes (convertir avec `dos2unix`).

Options globales : `--config CHEMIN`, `--dry-run` (aucune commande qui modifie quoi que ce soit, état non écrit).

## Configuration

Fichier TOML, voir [`examples/updates.example.toml`](examples/updates.example.toml) (placeholders à remplacer).
Il doit appartenir à `root` et ne pas être modifiable par d'autres : elle désigne les piles compose que l'outil pilote en root, et
l'outil **refuse de démarrer** sinon.

**`[settings]`** : `state_file`, `lock_file`, `registry_tool` (`buildx` ou `regctl`), `notify_url_file`, `notify_format`
(`discord`, `ntfy`, `json`), `default_cooldown`, `window`, `settle_seconds`, `health_timeout`,
`github_api`, `github_token_file`, `advisory_poll`, `advisory_lookback`, `version_label`, `cleanup`.

**`[[service]]`** : `name`, `compose_dir`, `service` (obligatoires) ; `compose_files`, `mode` (défaut `notify`, le plus
prudent), `cooldown`, `group`, `health_timeout`, `window`, `cleanup`, `smoke_url` +
`smoke_expect`, `advisories`, `min_severity` (défaut `high`), `security_cooldown` (défaut `6h`), `security_policy`
(`fast` ou `alert`), `release_notes` (défaut `false`), `release_notes_repos`, `release_notes_ignore`. Les fautes de
frappe dans les clés sont refusées.

## Comment la veille de sécurité décide

1. Pour chaque dépôt listé dans `advisories`, l'outil lit `GET /repos/{dépôt}/security-advisories` (avis **publiés**, ETag,
   au plus une requête par `advisory_poll`). Un échec de l'API n'empêche jamais les mises à jour normales.
2. Il garde les avis récents (`advisory_lookback`), non retirés, de sévérité ≥ `min_severity`.
3. Il lit la version de l'image **en cours** (étiquette `org.opencontainers.image.version`). Absente ou illisible → alerte.
4. Si cette version est dans une plage vulnérable de l'avis et qu'une image candidate existe, il la **télécharge sans
   l'exécuter** pour lire sa version. Si elle est **sortie** de toutes les plages : correctif confirmé. Sinon : alerte, délai normal.
5. Correctif confirmé et politique `fast` en mode `auto` → prêt après `security_cooldown`, compté depuis la plus tardive
   de la première observation de l'image et de la publication de l'avis. Un avis tout frais n'est donc jamais instantané.
6. Une version « à jour mais vulnérable » (aucune image corrigée n'existe encore) déclenche une alerte, une seule fois.

## Notifications

Un passage `run` (ou `now`) envoie **au plus UN message**, quel que soit le nombre d'applications suivies :

| Section | Contenu |
| --- | --- |
| 🚨 Échecs de mise à jour | un service ne repart pas : aucun retour arrière, images précédentes nommées |
| ⚠️ Sécurité (avis) | correctif détecté, version vulnérable, cas ambigu ; avec le lien de la release qui cite l'avis |
| 📝 Notes de release | releases plus récentes qui mentionnent un motif de sécurité (alertes seulement) |
| ⚠️ Erreurs de vérification | registre injoignable, compose illisible… (une fois par erreur) |
| ✅ Mises à jour réussies | `service → image` |
| 📦 Nouvelles versions prêtes | **tableau** service / image / vue depuis, pour les services en mode `notify` |
| ℹ️ Infos | signaux faibles, « impossible de comparer » |
| 📋 État des services | **tableau** service / état / détail, uniquement sur demande : `check [NOM…] --notify` |

**Scan manuel.** `docker-update check` scanne tout ; `docker-update check traefik immich` ne scanne que ces services (un nom de
groupe désigne ses membres). Une sélection ne touche ni aux compteurs de délai ni aux erreurs connues des autres services.
`check --notify` envoie en plus le tableau d'état (à jour, prête, différée, à revoir, ignoré, ERREUR) ; sans URL lisible, il
échoue avec un message clair. `run NOM…` fait de même en appliquant ce qui est prêt, pour la sélection seulement.

Le niveau du message (✅ ℹ️ ⚠️ 🚨) est celui du pire événement. Rien de nouveau : **aucun message**. Le message est borné à
environ 1 650 caractères : le plus important vient en premier, le reste est compté (« … N élément(s) non affiché(s) ») et
reste dans le journal. Les mentions (`@everyone`…) sont neutralisées. `check` n'envoie jamais rien.

L'URL du webhook se règle avec `docker-update set-webhook` : saisie masquée (ou entrée standard), adresse validée (webhook
Discord), écriture atomique dans `notify_url_file` en `600`, jamais affichée ni passée en argument de ligne de commande.

## Veille des notes de release (alertes seulement)

Activée service par service avec `release_notes = true`. Elle ne fait **qu'alerter** : une note de release est un texte
écrit par un tiers, donc non fiable. Elle ne raccourcit jamais le délai, ne rend rien éligible, ne tire aucune image.

1. Dépôt lu : `release_notes_repos` (liste « propriétaire/dépôt »), à défaut `advisories`. L'outil lit
   `GET /repos/{dépôt}/releases`, avec les mêmes ETag, le même jeton (`github_token_file`) et le même `advisory_poll`
   que les avis. La première lecture demande les 30 dernières releases (l'historique) ; ensuite seulement les 10 dernières
   (on ne cherche que les nouveautés, et un 304 ne renvoie rien) ; 30 de nouveau après 7 jours sans lecture complète. Un échec de l'API (réseau, limite de débit, 404 : dépôt sans release ou privé) n'est
   jamais une erreur et n'empêche jamais les mises à jour.
2. Version en cours : étiquette `org.opencontainers.image.version` (ou `version_label`). Absente ou illisible
   (`latest`, `main`…) : **une seule alerte « impossible de comparer »**, aucune note lue, aucune requête.
3. Sont considérées les releases **strictement plus récentes** que la version en cours. Les brouillons et les
   pré-versions (drapeau GitHub ou étiquette `-rc1`) sont toujours ignorés, ainsi que les étiquettes illisibles.
4. Motifs **forts** (mots entiers, insensibles à la casse, bornés, 64 premiers Kio de chaque note) : `CVE-AAAA-NNNN+`,
   `GHSA-xxxx-xxxx-xxxx`, `security fix(es)/update(s)/release(s)/patch(es)/advisory`, `vulnerab…`, `RCE`, `XSS`, `SSRF`,
   `CSRF`, `SQL injection`, `privilege escalation`, `auth… bypass`, `path traversal`. Alerte de niveau « avertissement ».
   Le mot **`security` seul** est un signal **faible** : alerte « info », **regroupée** (un message par service et par passage).
5. `release_notes_ignore = ["npm audit", …]` : expressions régulières (insensibles à la casse) ; un motif trouvé dont
   l'extrait correspond à l'une d'elles est écarté. Utile pour les notes énormes ou bruyantes (ex. `nodejs/node`).
6. **Une alerte par (service, étiquette de release)**, mémorisée dans le fichier d'état (conservée 120 jours). Elle donne
   le service, `version en cours → release`, les motifs, un extrait nettoyé et borné, et le lien de la release (reconstruit
   par l'outil, jamais copié depuis la réponse). Aucune mention Discord possible.
7. **Avis et notes se complètent.** Si une note cite un avis (GHSA ou CVE) que le service suit déjà via `advisories` (avis
   récent, non retiré, de sévérité suffisante), il n'y a **pas de second message** : l'alerte de l'avis reçoit la ligne
   « Notes de release qui le citent : … » avec le lien. Une note qui cite en plus autre chose garde son alerte, limitée
   à ce qui est nouveau, avec « Déjà couvert par l'avis : … ». L'accélération reste décidée par l'**avis** seul. Limite :
   le lien n'est ajouté qu'aux alertes d'avis envoyées après la publication de la release.
8. `check` journalise ces alertes (et la version détectée) sans rien envoyer ni mémoriser ; `run` notifie.

Limites : la comparaison est numérique sur `X.Y[.Z]` ; une release d'une autre branche de maintenance plus récente que la
vôtre (ex. 9.0.1 alors que vous suivez 8.1) est signalée aussi. Une note qui ne contient aucun des motifs ne déclenche rien
(absence d'alerte ≠ absence de faille). Les images sans étiquette de version ne peuvent pas être comparées.

## Modèle de menace, en bref

- **Aucun droit Docker pour un conteneur.** Ni socket-proxy, ni liste de montages à maintenir. Le script s'exécute en
  root (`systemd`, durci) et n'ouvre aucun port.
- **Pas de shell.** Toutes les commandes sont des listes d'arguments ; une valeur de configuration ne devient jamais une commande.
- **Entrées extérieures traitées comme non fiables** : résumés d'avis et notes de release nettoyés et bornés (jamais
  d'accélération depuis une note), mentions Discord désactivées,
  digest validé, manifeste borné, versions et plages lues strictement (jamais devinées).
- **Secrets** : URL du webhook, jeton GitHub et identifiants du registre sont lus dans des fichiers `600`, jamais journalisés.
  Le contenu de `docker compose config` (qui contient les variables d'environnement) n'est jamais écrit dans les journaux.
- **Ce que l'outil ne protège pas** : une image compromise publiée par l'éditeur *et* restée inchangée pendant le délai ;
  une mise en production par un compte root compromis. Le délai réduit le premier risque, il ne le supprime pas.

## Limites connues

- **Jamais testé contre un vrai démon Docker** (aucun n'était disponible pendant le développement). Les commandes ont été
  vérifiées avec Docker 29.8.2 / Compose 5.6.0 (options, format de `config --format json`, lecture du manifeste brut sur un
  registre public), le reste avec un faux `docker`. La première exécution réelle doit être `check`, puis un service factice.
- Pas de retour arrière automatique : après un échec, le service reste dans l'état où il est (éventuellement arrêté ou
  en boucle) jusqu'à votre intervention. Surveillez les alertes « ÉCHEC mise à jour ».
- Le digest comparé est celui de l'index de l'étiquette ; si `check` annonce « nouvelle image » pour **tous** les services
  dès la première fois, c'est que la comparaison ne correspond pas à votre moteur : s'arrêter (voir `docs/AGENT.md`).
- Le nettoyage ne supprime que l'image remplacée. Une image téléchargée seulement pour lire sa version (veille de
  sécurité) puis jamais appliquée reste sur le disque : `docker image prune` à la main de temps en temps.
- La veille de sécurité ne couvre que les projets qui publient leurs avis sur GitHub, et que les images qui portent leur version en étiquette.
- Docker Hub : lire un manifeste peut compter comme un téléchargement (limites anonymes). `regctl` n'utilise que des requêtes `HEAD`.

## Tests et qualité

```sh
go vet ./... && go test -race -count=1 ./...
```

Les tests unitaires couvrent chaque paquet. Les tests de **bout en bout** (`e2e/`) lancent le **vrai binaire** en sous-processus, avec un faux
exécutable `docker` (`testdata/fakedocker`) placé dans le `PATH` et de vrais serveurs HTTP locaux (avis, releases, webhook). Aucun test
n'appelle un vrai Docker, GitHub ou Discord. La CI vérifie aussi le formatage (`gofmt`), compile pour linux amd64 et arm64 et fait un fuzz
court du lecteur TOML.

Le code vient d'une réécriture d'une version Python (dernier état : commit `50952f1`), vérifiée contre elle par un test différentiel
(mêmes scénarios, mêmes sorties, état et requêtes) avant sa suppression. `docs/PORTAGE.md` garde l'inventaire des tests portés, les écarts
et les propositions d'amélioration non appliquées.

**Tests de mutation manuels.** Défauts introduits un par un dans le code Go (suite relancée à chaque fois, source restaurée ensuite), 57 au
total : délai de sécurité ou fenêtre ignorés, tirage par étiquette, note de release qui accélère, pré-version ou signal faible mal
traités, alertes non dédoublonnées, `@everyone` non neutralisé, jeton ou URL journalisés, conteneur arrêté touché, nettoyage forcé ou après
un échec, `check NOM` ignoré, une sélection qui purge l'état des autres, droits ou hôte du webhook, budget du rapport… 56 sont
détectés (54 d'emblée, 2 après l'ajout d'un test : pause d'un groupe, simulation qui n'attend pas) et 1 est équivalent (une garde
redondante dans `security.go`). Ils ne sont pas rejoués par la CI ; le détail est dans `docs/PORTAGE.md`.

Licence : à choisir par le propriétaire du dépôt.
