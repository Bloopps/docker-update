## Installer ou mettre à jour

```sh
sudo docker-update update          # installe cette release (SHA256SUMS vérifié, binaire seul remplacé)
docker-update validate             # signale un ancien format de configuration ou d'unité systemd
sudo docker-update migrate         # seulement si validate le demande (aperçu, confirmation, sauvegarde)
```

Première installation : télécharger `docker-update-linux-amd64` (ou `-arm64`), vérifier avec `sha256sum -c SHA256SUMS --ignore-missing`, puis `sudo ./docker-update-linux-amd64 init`.

Les PR portant l'étiquette `migration` figurent en tête de la liste ci-dessous : lire leur description avant de mettre à jour.

## Changements
