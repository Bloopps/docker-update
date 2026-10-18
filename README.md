# docker-update

Mises à jour automatiques d'images **Docker Compose**, sans conteneur qui détienne le droit de créer d'autres conteneurs.

Un outil en Python (bibliothèque standard seulement, Python ≥ 3.11) qui tourne **sur l'hôte**, lancé par un minuteur `systemd`.
Pensé comme une alternative plus sûre à Watchtower pour un serveur personnel.

## Ce qu'il fait

- **Délai de sécurité mesuré par l'outil.** Une image n'est appliquée que si son *digest* est resté le même pendant
  `cooldown` (3 jours par défaut), compté depuis la première fois que l'outil l'a **vu**. Une date écrite dans l'image,
  qu'un éditeur peut fausser, n'entre pas en jeu. Si l'étiquette bouge avant la fin, le compteur repart.
- **Veille de sécurité.** Il lit les avis de sécurité publiés par les dépôts GitHub des projets suivis. Si un avis
  touche la version **en cours** et que l'image candidate (version lue dans ses étiquettes) n'est **plus** touchée,
  le délai est ramené à `security_cooldown` (6 h par défaut). Tout cas ambigu devient une **alerte**, jamais une accélération.
- **Application exacte.** Il tire le digest évalué (pas « l'étiquette », qui a pu bouger), retague localement, puis
  recrée le service avec `docker compose up -d --no-deps --pull never --wait`.
- **Retour arrière.** Si le service ne repart pas (sonde de santé, état après démarrage, test de fumée HTTP), retour à
  l'image précédente de chaque service, et alerte. Désactivable par service (`rollback = false`) quand une base a migré.
- **Fenêtre horaire** (`02:00-05:00`) : on observe tout le temps, on n'applique que dans la fenêtre. Un avis *critique* l'ignore.
- **Groupes** : plusieurs services d'une même image mis à jour ensemble, dans l'ordre du fichier.
- **Crochet avant mise à jour** (ex. sauvegarde de la base) : s'il échoue, rien n'est touché.
- **Modes par service** : `auto` (applique), `notify` (prévient, vous appliquez), `manual` (ignore).
- **Notifications Discord** (ntfy ou JSON possibles), sans mention possible, sans doublons, sans jamais journaliser l'URL.
- **Signal de vie** optionnel (par ex. une URL « push » d'Uptime Kuma) pour savoir si l'outil lui-même s'arrête en silence.
- **Pause** (`hold`), **voie rapide** (`now`), **simulation** (`--dry-run`), verrou contre deux passages simultanés.

## Sous-commandes

| Commande | Effet |
| --- | --- |
| `validate` | vérifie configuration, outils et chemins ; ne touche à rien |
| `check` | observe (registre, état, tests de fumée, version détectée) ; n'applique **rien**, n'envoie **rien**, ne télécharge **rien** |
| `run` | observe, applique ce qui est prêt en mode `auto`, notifie |
| `now NOM… --yes` | applique tout de suite (sans délai ni fenêtre) ; sans `--yes`, simule |
| `hold NOM…` / `unhold NOM…` | met en pause / reprend |
| `status` | état mémorisé |
| `notify-test` | envoie un message de test (vérifie le webhook) |

Options globales : `--config CHEMIN`, `--dry-run` (aucune commande qui modifie quoi que ce soit, état non écrit).

## Configuration

Fichier TOML, voir [`examples/updates.example.toml`](examples/updates.example.toml) (placeholders à remplacer).
Il doit appartenir à `root` et ne pas être modifiable par d'autres : il contient des commandes exécutées en root, et
l'outil **refuse de démarrer** sinon.

**`[settings]`** : `state_file`, `lock_file`, `registry_tool` (`buildx` ou `regctl`), `notify_url_file`, `notify_format`
(`discord`, `ntfy`, `json`), `heartbeat_url_file`, `default_cooldown`, `window`, `settle_seconds`, `health_timeout`,
`github_api`, `github_token_file`, `advisory_poll`, `advisory_lookback`, `version_label`.

**`[[service]]`** : `name`, `compose_dir`, `service` (obligatoires) ; `compose_files`, `mode` (défaut `notify`, le plus
prudent), `cooldown`, `group`, `rollback` (défaut `true`), `health_timeout`, `pre_hook`, `window`, `smoke_url` +
`smoke_expect`, `advisories`, `min_severity` (défaut `high`), `security_cooldown` (défaut `6h`), `security_policy`
(`fast` ou `alert`). Les fautes de frappe dans les clés sont refusées.

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

## Modèle de menace, en bref

- **Aucun droit Docker pour un conteneur.** Ni socket-proxy, ni liste de montages à maintenir. Le script s'exécute en
  root (`systemd`, durci) et n'ouvre aucun port.
- **Pas de shell.** Toutes les commandes sont des listes d'arguments ; une valeur de configuration ne devient jamais une commande.
- **Entrées extérieures traitées comme non fiables** : résumés d'avis nettoyés et bornés, mentions Discord désactivées,
  digest validé, manifeste borné, versions et plages lues strictement (jamais devinées).
- **Secrets** : URL du webhook, jeton GitHub et identifiants du registre sont lus dans des fichiers `600`, jamais journalisés.
  Le contenu de `docker compose config` (qui contient les variables d'environnement) n'est jamais écrit dans les journaux.
- **Ce que l'outil ne protège pas** : une image compromise publiée par l'éditeur *et* restée inchangée pendant le délai ;
  une mise en production par un compte root compromis. Le délai réduit le premier risque, il ne le supprime pas.

## Limites connues

- **Jamais testé contre un vrai démon Docker** (aucun n'était disponible pendant le développement). Les commandes ont été
  vérifiées avec Docker 29.8.2 / Compose 5.6.0 (options, format de `config --format json`, lecture du manifeste brut sur un
  registre public), le reste avec un faux `docker`. La première exécution réelle doit être `check`, puis un service factice.
- Un retour arrière **ne défait pas** les migrations de base de données : d'où `rollback = false` pour la tirelire.
- Le digest comparé est celui de l'index de l'étiquette ; si `check` annonce « nouvelle image » pour **tous** les services
  dès la première fois, c'est que la comparaison ne correspond pas à votre moteur : s'arrêter (voir `docs/AGENT.md`).
- La veille de sécurité ne couvre que les projets qui publient leurs avis sur GitHub, et que les images qui portent leur version en étiquette.
- Docker Hub : lire un manifeste peut compter comme un téléchargement (limites anonymes). `regctl` n'utilise que des requêtes `HEAD`.

## Tests et qualité

```sh
python3 -m unittest discover -s tests -v      # 114 tests : logique, faux monde Docker, bout en bout avec un faux `docker`
```

Les tests de bout en bout lancent le **vrai programme** en sous-processus, avec un faux exécutable `docker` placé dans le
`PATH` et un vrai serveur HTTP local pour les avis de sécurité.

Un **test de mutation** manuel a introduit 38 défauts un par un (délai ignoré, tirage par étiquette, retour arrière absent,
fenêtre ignorée, jeton journalisé, avis retiré pris en compte, plage illisible comptée comme corrigée…). Les 38 sont
détectés ; il a aussi révélé trois angles morts des tests, comblés, et un vrai défaut de code (retour arrière de groupe).
Il n'est pas rejoué par la CI.

## Installation

Voir [`docs/AGENT.md`](docs/AGENT.md) : procédure pas à pas, avec ce qui se vérifie en lecture seule, les critères
d'acceptation de chaque étape et le retour arrière.

Licence : à choisir par le propriétaire du dépôt.
