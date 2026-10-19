# docker-update

Met à jour vos images **Docker Compose** avec un **délai de sécurité** : une nouvelle image n'est appliquée qu'après 3 jours sans
changement, ou 6 heures si elle corrige un avis de sécurité qui touche votre version. Un seul binaire, lancé sur le serveur par un
minuteur `systemd`. Aucun conteneur n'a accès au socket Docker (contrairement à Watchtower).

## Installation

Prérequis : `docker` avec le plugin `compose`. Téléchargez le binaire depuis la page « Releases » du dépôt, puis :

```sh
chmod +x docker-update-linux-amd64 && sudo ./docker-update-linux-amd64 init
```

`init` vérifie le serveur, installe le binaire et le minuteur, vous demande quels conteneurs suivre et écrit la configuration. Il
demande confirmation avant chaque modification et ne met rien à jour : tous les services démarrent en mode `notify` (l'outil vous
prévient, vous appliquez).

## Usage

```sh
docker-update list                # services suivis
docker-update check               # observer, sans rien appliquer
docker-update now NOM --yes       # appliquer tout de suite
docker-update add NOM --container NOM     # suivre un conteneur de plus (rename, edit, delete existent aussi)
sudo docker-update update         # se mettre à jour depuis la dernière release (SHA256SUMS vérifié ; --check : voir sans installer)
sudo docker-update migrate        # si validate signale un ancien format de configuration ou d'unité systemd
```

## Aller plus loin

[`docs/REFERENCE.md`](docs/REFERENCE.md) (commandes, configuration, veilles, notifications, limites) ·
[`docs/AGENT.md`](docs/AGENT.md) (déploiement pas à pas) · [`examples/`](examples/) · développement : `go vet ./... && go test -race ./...`
