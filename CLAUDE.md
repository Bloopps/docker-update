# docker-update

Outil Go (bibliothèque standard seulement, un binaire statique linux amd64 / arm64) de mise à jour d'images Docker Compose,
exécuté sur l'hôte. Voir README.md (installation, fonctionnement, limites) et docs/AGENT.md (procédure de déploiement).

## Commandes

- Tests (tous) : `go vet ./... && go test -race -count=1 ./...`
- Un test : `go test -count=1 -run TestSecurity_FastTrackWaitsSecurityCooldownThenApplies ./internal/updater`
- Bout en bout seulement : `go test -count=1 ./e2e` (compile le binaire et le faux `docker`)
- Format : `gofmt -l .` ne doit rien afficher
- Compilation : `CGO_ENABLED=0 go build -trimpath -o docker-update ./cmd/docker-update`

## Règles du projet

- Aucune dépendance tierce (`go.mod` sans `require`), aucun shell : les commandes sont toujours des listes d'arguments.
- Tout texte venu de l'extérieur (avis GitHub, sorties de commandes) est nettoyé et borné avant d'être journalisé ou notifié.
- Ne journalise jamais : URL du webhook, jeton GitHub, sortie de `docker compose config` (elle contient l'environnement).
- Les tests n'appellent jamais un vrai Docker, GitHub ou Discord (faux `docker`, `httptest`, 127.0.0.1).
- Une fonctionnalité qui peut accélérer ou appliquer une mise à jour doit avoir un test qui échoue quand on la casse
  (test de mutation manuel : introduire le défaut, vérifier que la suite le détecte).
- Aucun service en `auto` dans un exemple sans l'accord explicite du propriétaire.
- La documentation et les messages sont en français.
