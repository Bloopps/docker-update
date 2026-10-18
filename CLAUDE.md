# docker-update

Outil Python (bibliothèque standard seulement, Python >= 3.11) de mise à jour d'images Docker Compose, exécuté sur
l'hôte. Voir README.md (fonctionnement, limites) et docs/AGENT.md (procédure de déploiement).

## Commandes

- Tests (tous) : `python3 -W error::ResourceWarning -m unittest discover -s tests -v`
- Un test : `python3 -m unittest tests.test_docker_update.TestSecurity.test_fast_track_waits_security_cooldown_then_applies`
  (ou `cd tests && python3 -m unittest test_docker_update.TestSecurity`)
- Syntaxe du crochet : `bash -n deploy/tirelire-predump.sh`

## Règles du projet

- Aucune dépendance tierce, aucun shell : les commandes sont toujours des listes d'arguments.
- Tout texte venu de l'extérieur (avis GitHub, sorties de commandes) est nettoyé et borné avant d'être journalisé ou notifié.
- Ne journalise jamais : URL du webhook, jeton GitHub, sortie de `docker compose config` (elle contient l'environnement).
- Une fonctionnalité qui peut accélérer ou appliquer une mise à jour doit avoir un test qui échoue quand on la casse
  (test de mutation manuel : introduire le défaut, vérifier que la suite le détecte).
- La documentation et les messages sont en français.
