# docker-update

Met à jour vos images **Docker Compose** toute seule, avec un **délai de sécurité**. Un seul binaire Go, sans dépendance, lancé
sur le serveur par un minuteur `systemd`. Alternative plus sûre à Watchtower : aucun conteneur n'a accès au socket Docker.

## Comment ça marche

- Une nouvelle image n'est appliquée qu'après **3 jours** sans changement (réglable). Le compteur est celui de l'outil, pas celui de l'éditeur.
- Si un avis de sécurité GitHub touche votre version et que la nouvelle image le corrige, le délai tombe à **6 heures**.
- Trois modes par service : `notify` (vous prévient, vous appliquez, **défaut**), `auto` (applique seul), `manual` (ignore).
- Si un service ne repart pas après une mise à jour, l'outil **alerte et s'arrête**. Il ne fait aucun retour arrière à l'aveugle.
- Un seul message Discord par passage, et rien s'il n'y a rien de nouveau.

## Installation

Il faut `docker` avec le plugin `compose` sur le serveur (Linux amd64 ou arm64). Le reste, c'est l'outil qui s'en occupe :

```sh
# Télécharger docker-update-linux-amd64 (ou -arm64) depuis la page « Releases » du dépôt, puis :
chmod +x docker-update-linux-amd64
sudo ./docker-update-linux-amd64 init
```

Ensuite, `sudo docker-update update` installe la dernière release (somme SHA256 vérifiée, binaire seul remplacé ; `--check` pour voir sans installer). Chaque fusion sur `main` publie une release.

`init` fait tout, dans l'ordre, en vous demandant avant chaque modification :

1. **Vérifie** le serveur : Docker et `compose`, droits, accès au registre et à GitHub. Ce qui bloque est expliqué avec la commande qui le corrige.
2. **Installe** le binaire dans `/usr/local/sbin`, crée les dossiers, écrit les unités `systemd` (jamais écrasées si elles existent déjà).
3. **Relève** vos conteneurs en marche et vous demande, service par service, lesquels suivre.
4. **Écrit** `/etc/docker-update/updates.toml` après votre confirmation, propose d'enregistrer votre webhook Discord, puis **active le minuteur**.

Rien n'est mis à jour pendant `init`, et chaque service est en mode `notify` (l'outil vous prévient, vous appliquez). Le minuteur ne fait
qu'**observer** tant que vous n'avez pas décidé de passer à `run` ([`docs/AGENT.md`](docs/AGENT.md)). `init --check` rejoue les
vérifications seules, à tout moment. Pour compiler soi-même : `CGO_ENABLED=0 go build -trimpath -o docker-update ./cmd/docker-update` (Go 1.27 ou plus récent).

## Au quotidien

```sh
docker-update list                  # vos services suivis
docker-update check                 # observer, sans rien appliquer
docker-update now traefik --yes     # appliquer tout de suite un service
docker-update hold traefik          # le mettre en pause (unhold pour reprendre)
docker-update add monapp --container monapp     # suivre un conteneur de plus
docker-update rename monapp app     # renommer ; delete pour retirer ; edit pour tout modifier dans $EDITOR
docker-update validate --online     # vérifier la configuration, les outils et les dépôts GitHub
```

`add`, `rename` et `delete` modifient le fichier de configuration sans toucher à vos commentaires : aperçu des changements,
confirmation, sauvegarde automatique, validation. Ils ne touchent jamais à un conteneur. `--dry-run` simule n'importe quelle commande.

## Pour aller plus loin

- [`docs/REFERENCE.md`](docs/REFERENCE.md) : toutes les commandes et options, la configuration, la veille de sécurité, les notes de release, les notifications, le modèle de menace, les limites.
- [`examples/updates.example.toml`](examples/updates.example.toml) : exemple de configuration.
- [`docs/AGENT.md`](docs/AGENT.md) : déploiement pas à pas, avec critères d'acceptation et retour arrière.
- [`docs/PORTAGE.md`](docs/PORTAGE.md) : historique de la réécriture Go et propositions en attente.

## Développement

```sh
go vet ./... && go test -race -count=1 ./...
```

Aucun test n'appelle un vrai Docker, GitHub ou Discord (faux `docker`, serveurs HTTP locaux).

Licence : à choisir par le propriétaire du dépôt.
