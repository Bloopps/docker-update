# docker-update

Met à jour vos images **Docker Compose** avec un **délai de sécurité** : une nouvelle image n'est appliquée qu'après 3 jours sans
changement, ou 6 heures si elle corrige un avis de sécurité qui touche votre version. Un seul binaire, lancé sur le serveur par un
minuteur `systemd`. Aucun conteneur n'a accès au socket Docker (contrairement à Watchtower).

## Installation

Prérequis : Linux (amd64 ou arm64) avec `docker` et le plugin `compose`.

```sh
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
gh release download --repo Bloopps/docker-update --pattern "docker-update-linux-$arch" --output docker-update
chmod +x docker-update && sudo ./docker-update init
```

Le dépôt est privé : il faut la CLI [`gh`](https://cli.github.com/) connectée (`gh auth login`). Si le dépôt devient public, `curl -fsSL -o docker-update https://github.com/Bloopps/docker-update/releases/latest/download/docker-update-linux-$arch` suffit.

`init` vérifie le serveur, installe le binaire et le minuteur, vous demande quels conteneurs suivre et écrit la configuration. Il
demande confirmation avant chaque modification et ne met rien à jour : tous les services démarrent en mode `notify` (l'outil vous
prévient, vous appliquez). Pour mettre l'outil à jour plus tard : `sudo env GH_TOKEN=$(gh auth token) docker-update update` (sans `GH_TOKEN` si le dépôt est public).

## Usage

```sh
docker-update list                # services suivis
docker-update check               # observer, sans rien appliquer
docker-update now NOM --yes       # appliquer tout de suite
docker-update add NOM --container NOM     # suivre un conteneur de plus (rename, edit, delete existent aussi)
sudo env GH_TOKEN=$(gh auth token) docker-update update   # se mettre à jour depuis la dernière release (SHA256SUMS vérifié ; --check : voir sans installer)
sudo docker-update migrate        # si validate signale un ancien format de configuration ou d'unité systemd
```

## Aller plus loin

[`docs/REFERENCE.md`](docs/REFERENCE.md) (commandes, configuration, veilles, notifications, limites) ·
[`docs/AGENT.md`](docs/AGENT.md) (déploiement pas à pas) · [`examples/`](examples/) · développement : `go vet ./... && go test -race ./...`
