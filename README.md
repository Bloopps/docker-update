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

Il faut `docker` avec le plugin `compose` sur le serveur (Linux amd64 ou arm64).

```sh
# 1. Récupérer le binaire : release GitHub (docker-update-linux-amd64 ou -arm64 + SHA256SUMS), ou le compiler :
CGO_ENABLED=0 go build -trimpath -o docker-update ./cmd/docker-update      # Go 1.27 ou plus récent

# 2. Le poser et installer le minuteur
sudo install -m 755 docker-update /usr/local/sbin/docker-update
sudo install -m 644 deploy/docker-update.service deploy/docker-update.timer /etc/systemd/system/
sudo install -d -m 755 /etc/docker-update
sudo install -d -m 700 /var/lib/docker-update

# 3. Laisser l'outil vérifier l'installation et écrire la configuration à partir de vos conteneurs
sudo docker-update init
```

`init` contrôle tout (docker, droits, unités systemd, accès au registre et à GitHub), dit ce qui manque et comment le corriger, puis
liste vos conteneurs et vous demande, service par service, lesquels suivre. Il écrit `/etc/docker-update/updates.toml` après votre
confirmation, **ne lance aucune mise à jour** et laisse tout en `notify`. Il propose ensuite d'enregistrer votre webhook Discord.

Pour terminer : `sudo systemctl daemon-reload && sudo systemctl enable --now docker-update.timer`.
Le minuteur fourni ne fait qu'**observer** (`check`) tant que vous n'avez pas décidé de passer à `run`. La procédure complète est dans
[`docs/AGENT.md`](docs/AGENT.md).

## Au quotidien

```sh
docker-update list                  # vos services suivis
docker-update check                 # observer, sans rien appliquer
docker-update now traefik --yes     # appliquer tout de suite un service
docker-update hold traefik          # le mettre en pause (unhold pour reprendre)
docker-update add monapp --container monapp     # suivre un conteneur de plus
docker-update rename monapp app     # renommer, delete pour retirer
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
