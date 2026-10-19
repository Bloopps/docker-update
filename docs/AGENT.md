# Procédure de déploiement de `docker-update` (document pour l'agent)

Tu déploies `docker-update` sur le serveur de l'utilisateur, à la place de Watchtower. **Tu avances par étapes. Après chaque
étape tu rends compte et tu attends son accord avant la suivante**, sauf pour les étapes marquées « lecture seule ».

## 0. Règles non négociables

1. **Étapes 1 à 3 : lecture seule.** Tu ne modifies, ne démarres, ne redémarres ni n'arrêtes rien. À partir de l'étape 4 chaque
   action qui modifie le système demande l'accord explicite de l'utilisateur, étape par étape.
2. **Aucune installation involontaire.** N'utilise jamais `lxc`. Si une commande déclenche une installation, un téléchargement
   inattendu ou une modification que tu n'avais pas annoncée : **arrête-toi**, dis-le, ne corrige pas de toi-même.
3. **Aucun secret affiché.** Ne lis ni n'affiche le contenu de `secrets/`, des `.env`, des `.pem`, des sauvegardes SQL, des
   jetons, de l'URL du webhook. Pour un `.env` ou un `environment:` : les NOMS des variables seulement.
   L'URL du webhook Discord et les jetons sont fournis par l'utilisateur ; tu les écris dans leur fichier (`600`) **sans les
   répéter** dans ta réponse, tes journaux, un commit ou un message.
4. **Une seule chose à la fois.** Si une sortie diffère de ce qui est attendu, **décris-la et arrête-toi**. Ne cherche pas de contournement.
5. **Garde l'ancien système.** Watchtower est **arrêté, jamais supprimé**, pendant au moins une semaine après la bascule.
6. **Mémorise ce que tu fais.** Note chaque commande lancée et précise lesquelles ont modifié quelque chose.

Condition d'arrêt générale : tout comportement qui ressemble à un danger pour un service en production → stop et rapport.

## 1. Contenu du dépôt

| Fichier | Rôle |
| --- | --- |
| `docker-update.py` | l'outil (Python ≥ 3.11, bibliothèque standard) |
| `examples/updates.example.toml` | configuration d'exemple (placeholders à remplacer) |
| `examples/updates.watchtower.toml` | les applications suivies par Watchtower, toutes en `notify` (placeholders à remplacer) |
| `deploy/docker-update.service`, `.timer` | unités `systemd` (le service démarre en mode **`check`**, qui n'applique rien) |
| `tests/` | 190 tests (`python3 -m unittest discover -s tests`) |

Lis d'abord `README.md` (fonctionnement, sous-commandes, modèle de menace, limites connues).

## 2. Étape 1 — Pré-vérifications (lecture seule)

```sh
python3 --version                                   # >= 3.11 requis (tomllib)
docker --version ; docker compose version ; docker buildx version
docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}'
```

Teste l'outil lui-même sans rien installer, depuis le dépôt cloné :

```sh
python3 -m unittest discover -s tests 2>&1 | tail -5
```
**Attendu : « OK », 190 tests.** Les tests ne créent que des fichiers temporaires et un serveur HTTP local sur `127.0.0.1`.
Si un test échoue, ne continue pas : donne la sortie.

Vérifie que la lecture du digest distant fonctionne **depuis ce serveur** (c'est la commande exacte que l'outil utilise) :

```sh
docker buildx imagetools inspect traefik:v3.6 --raw | head -c 300      # adapte à une étiquette réelle utilisée
docker buildx imagetools inspect traefik:v3.6 --raw | sha256sum         # doit être le digest annoncé par le registre
```
Attendu : un JSON de manifeste. Si le registre répond `429` (Docker Hub), note-le : c'est la limite anonyme, voir la section Dépannage.

## 3. Étape 2 — Relevé des piles (lecture seule) et rédaction de la configuration

Pour **chaque** service à suivre, relève le dossier compose, le nom du service, les fichiers compose et le nom du projet :

```sh
for c in $(docker ps -q); do
  docker inspect --format '{{.Name}} | projet={{index .Config.Labels "com.docker.compose.project"}} | service={{index .Config.Labels "com.docker.compose.service"}} | dossier={{index .Config.Labels "com.docker.compose.project.working_dir"}} | fichiers={{index .Config.Labels "com.docker.compose.project.config_files"}} | image={{.Config.Image}}' $c
done
```

Pour chaque service retenu, **contrôle que l'outil retrouvera bien le même conteneur** :

```sh
cd <dossier> && docker compose -f <fichiers…> config --format json | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("name"))'
```
Le nom affiché **doit être identique** à la valeur `projet=` du conteneur. S'il diffère, l'outil ne retrouvera pas le conteneur
(il l'ignorera, sans rien créer) : note-le et ne mets pas ce service en `auto`. **N'affiche jamais** la sortie complète de
`config` : elle contient les variables d'environnement.

Produis `updates.toml` à partir de `examples/updates.example.toml` en remplaçant TOUS les placeholders (`/home/ubuntu/...`,
`EXEMPLE.fr`, noms de services). Règles de répartition à respecter :

| Service | Mode | Remarques |
| --- | --- | --- |
| traefik, pocket-id, crowdsec | `auto` | délai 3 j, fenêtre 02:00-05:00, `advisories`, test de fumée |
| homepage, beszel, goaccess | `auto` | délai 3 j, `window = ""` |
| tirelire (migrate, egress, web, admin, sync) | `auto`, **groupe** `tirelire` | délai 1 j, `cleanup = false` (la sauvegarde de la base se gère ailleurs) |
| immich, portainer, wg-easy, portabase | `notify` | l'outil prévient, l'utilisateur applique avec `now` |
| watchtower, socket-proxy, la base `tirelire-db` | **ne pas les mettre** | |

Dans le groupe, l'ordre du fichier est l'ordre de mise à jour : `migrate` d'abord. Les services d'un groupe doivent avoir le même
mode, délai, `cleanup`, fenêtre et réglages de sécurité (l'outil le vérifie).

**Rends la configuration à l'utilisateur pour validation avant de continuer.**

## 4. Étape 3 — Installation (accord requis ; modifie le système)

Demande l'accord, puis :

```sh
sudo install -d -m 755 -o root -g root /etc/docker-update
sudo install -m 755 -o root -g root docker-update.py /usr/local/sbin/docker-update
sudo install -m 644 -o root -g root updates.toml /etc/docker-update/updates.toml        # la configuration validée
sudo install -m 644 -o root -g root deploy/docker-update.service deploy/docker-update.timer /etc/systemd/system/
```

Le dossier d'état et le dossier des identifiants du registre :

```sh
sudo install -d -m 700 -o root -g root /var/lib/docker-update /var/lib/docker-update/docker
```

Webhook Discord : l'utilisateur te donne l'URL. Écris-la dans un fichier `600` **sans l'afficher** :

```sh
sudo install -m 600 -o root -g root /dev/null /etc/docker-update/discord_webhook
# puis le contenu : une seule ligne, l'URL (via sudoedit ou `read -rs` ; jamais dans l'historique du shell ni dans un message)
```

Identifiants du registre : **seulement si** l'image de la tirelire sur GHCR est privée. Demande à l'utilisateur de créer un jeton
**nouveau**, avec la seule permission `read:packages`, jamais le jeton déjà utilisé par Watchtower :

```sh
DOCKER_CONFIG=/var/lib/docker-update/docker sudo -E docker login ghcr.io -u <utilisateur> --password-stdin
sudo chmod 600 /var/lib/docker-update/docker/config.json
```

Puis :

```sh
sudo systemctl daemon-reload
sudo /usr/local/sbin/docker-update --config /etc/docker-update/updates.toml validate
sudo /usr/local/sbin/docker-update --config /etc/docker-update/updates.toml notify-test      # un message doit arriver sur Discord
```
**Attendu :** `validate` termine par « OK » ; un message de test s'affiche dans Discord.

## 5. Étape 4 — Calibrage : premier `check` (aucune modification des services)

```sh
sudo env DOCKER_CONFIG=/var/lib/docker-update/docker /usr/local/sbin/docker-update --config /etc/docker-update/updates.toml check
```

**Critères d'acceptation, à vérifier un par un :**

1. **Les services sans nouveauté sont « à jour ».** Si TOUS les services annoncent « nouvelle image » dès le premier passage,
   la comparaison de digests ne correspond pas à ton moteur Docker : **STOP**. Donne, pour un service, la sortie de
   `docker image inspect --format '{{json .RepoDigests}}' <image>` et de `docker buildx imagetools inspect <ref> --raw | sha256sum`.
2. **Aucun service attendu n'est « ignoré : conteneur absent ».** Sinon, le nom du projet compose ne correspond pas : corrige la configuration.
3. **Chaque service avec `advisories` affiche « version détectée X ».** Si une ligne dit « version NON détectée », l'image ne porte
   pas l'étiquette de version : la veille de sécurité ne pourra rien conclure pour ce service. Note-le ; ne l'invente pas.
4. **Les tests de fumée sont « OK »** (`HTTP 302/401/403` selon la configuration). Un code inattendu veut dire que `smoke_expect`
   est mal calibré pour ce site **ou** que le serveur n'atteint pas son propre nom DNS (hairpin) : adapte-le, ne le désactive pas en silence.
5. **Chaque service avec `release_notes = true` affiche « notes de release … version détectée X ».** Si une ligne dit « version NON
   détectée », l'image n'a pas d'étiquette de version lisible (`latest`, `main`, absente) : la veille des notes ne pourra rien
   comparer (une seule alerte « impossible de comparer »). Note-le ; ne devine pas la version. Les lignes « ALERTE notes de
   release » de `check` sont informatives (rien n'est envoyé) : rapporte-les à l'utilisateur.
6. Aucun message Discord n'a été envoyé (`check` n'envoie rien) et aucun service n'a redémarré (`docker ps`).

Rends compte avec la sortie (sans secret).

## 6. Étape 5 — Essai sur un service factice, y compris un vrai échec (accord requis)

Cet essai exerce la mise à jour, le nettoyage **et un échec réel** (alerte, sans retour arrière) sans toucher un seul service réel.

1. Vérifie qu'aucune image locale `alpine:3.20` n'existe (`docker image ls alpine`). Sinon, choisis une autre petite image publique et dis-le.
2. Crée `/var/tmp/du-test/compose.yml` :
   ```yaml
   name: du-test
   services:
     dummy:
       image: alpine:3.20
       command: ["sleep", "3600"]
       restart: "no"
   ```
3. Fabrique une image « périmée » : `docker pull busybox:latest && docker tag busybox:latest alpine:3.20`, puis
   `cd /var/tmp/du-test && docker compose up -d`.
4. Configuration d'essai `/etc/docker-update/test.toml` (propriétaire root) : état et verrou séparés (`/var/lib/docker-update/test-state.json`,
   `/run/docker-update/test-lock`), `notify_url_file` vide, un service `dummy` (`compose_dir = "/var/tmp/du-test"`, `mode = "auto"`, `cooldown = "0"`, `window = ""`).
5. **Essai A : mise à jour réussie.** `docker-update --config …/test.toml check` → « nouvelle image … prête ». Puis
   `docker-update --config …/test.toml now dummy --yes`. **Attendu :** le conteneur `dummy` tourne maintenant sur l'image `alpine` réelle (l'ID d'image a changé), et le journal dit « nettoyage : image précédente … supprimée ».
6. **Essai B : échec réel, sans retour arrière.** Refais l'étape 3 (nouvelle image « périmée »). Ajoute au service `dummy` de `test.toml` :
   `smoke_url = "http://127.0.0.1:9/"` et `smoke_expect = [200]` (ce port ne répond pas : le test de fumée échoue forcément).
   `docker-update --config …/test.toml now dummy --yes`. **Attendu :** code de sortie 1, journal « test de fumée en échec »,
   le message « Aucun retour arrière automatique » qui nomme l'image précédente, le conteneur **resté** sur la nouvelle image
   (l'outil ne touche plus à rien), aucun nettoyage, et `status` montre l'image marquée en échec.
7. **Nettoyage obligatoire :** `cd /var/tmp/du-test && docker compose down`, `rm -rf /var/tmp/du-test`, `docker rmi alpine:3.20`,
   supprime `test.toml`, l'état et le verrou d'essai. Confirme avec `docker ps -a` et `docker images`.

Si l'essai A ou B ne donne pas le résultat attendu : **STOP**, rapport. N'active rien d'autre.

## 7. Étape 6 — Observation (accord requis)

1. `sudo systemctl enable --now docker-update.timer`. Le service lance `check` toutes les heures. **Rien n'est appliqué.**
2. Laisse tourner **48 heures**. Chaque jour : `journalctl -u docker-update --since "24 hours ago" --no-pager | tail -150`.
3. **Attendu :** aucune erreur répétée, les « différées » se transforment en « prête » après le délai, aucune surprise dans `status`.

## 8. Étape 7 — Bascule (accord requis, un seul changement à la fois)

Ne lance **jamais** `run` et Watchtower en même temps sur les mêmes services.

1. Mets d'abord en `notify` dans `updates.toml` les services de sécurité (traefik, pocket-id, crowdsec) : première semaine d'essai.
2. `sudo docker stop watchtower` (**stop, pas rm**). Si Watchtower suit encore des services que `docker-update` ne gère pas, dis-le.
3. Active `run` par un fichier de surcharge (ne modifie pas l'unité installée) :
   ```sh
   sudo systemctl edit docker-update.service
   # contenu :
   #   [Service]
   #   ExecStart=
   #   ExecStart=/usr/local/sbin/docker-update --config /etc/docker-update/updates.toml run
   ```
4. Surveille la première semaine. Après une semaine sans incident, **promeus** traefik/pocket-id/crowdsec en `auto` (fenêtre 02:00-05:00, délai 3 j).
5. Après une semaine de plus : retire définitivement Watchtower et ses socket-proxy, **avec l'accord explicite de l'utilisateur**.

Pour la tirelire : tant que `TIRELIRE_IMAGE` suit `:main`, garde un délai d'**au moins 1 jour**. Idéalement, l'utilisateur passe d'abord sur
un tag de version (`:1.2`) ; dans ce cas mets `cooldown = "0"` pour le groupe, la CI de l'utilisateur étant sa propre source de confiance.

## 9. Utilisation courante (à expliquer à l'utilisateur)

```sh
sudo docker-update --config /etc/docker-update/updates.toml status               # ce qui attend, depuis quand
sudo docker-update --config /etc/docker-update/updates.toml now traefik --yes    # voie rapide : applique tout de suite
sudo docker-update --config /etc/docker-update/updates.toml hold traefik         # met en pause (aucune mise à jour automatique)
sudo docker-update --config /etc/docker-update/updates.toml unhold traefik
sudo docker-update --config /etc/docker-update/updates.toml --dry-run run        # simulation complète
```
Conseil : un alias `du='sudo env DOCKER_CONFIG=/var/lib/docker-update/docker docker-update --config /etc/docker-update/updates.toml'`.

**Quand Discord annonce « Correctif de sécurité détecté »** : rien à faire, l'outil applique après `security_cooldown` (6 h) ; si c'est
critique, la fenêtre horaire est ignorée. **Quand Discord annonce « VULNÉRABLE »** : aucune image corrigée n'est encore publiée ; surveiller.
**Quand Discord annonce « Notes de release à lire »** : une release plus récente que la version en cours mentionne un motif de
sécurité (CVE, GHSA, « security fix »…). C'est une alerte seulement : rien n'est accéléré. L'utilisateur lit la note (lien dans le
message) et décide, puis `now <service> --yes`. Un message « mot *security* » (info) est un signal faible, sans CVE annoncé.
**Quand Discord annonce « Impossible de comparer »** : la version en cours n'est pas lisible, aucune note n'est analysée pour ce service.
**Quand Discord annonce « plage de versions illisible » ou « version … pas lisible »** : l'outil n'a pas pu conclure, à l'utilisateur de juger,
puis `now <service> --yes` s'il décide d'appliquer.

## 10. Retour arrière de tout le dispositif

```sh
sudo systemctl disable --now docker-update.timer
sudo docker start watchtower
```
Les services restent tels quels. L'état est dans `/var/lib/docker-update/state.json` (supprimable sans risque : les compteurs de délai repartent de zéro).

## 11. Dépannage

| Symptôme | Cause probable | Action |
| --- | --- | --- |
| `configuration : … doit appartenir à root` | fichier modifiable par d'autres | `sudo chown root:root` + `chmod 644` |
| « digest distant introuvable » | `429` Docker Hub, jeton absent pour GHCR, ou `buildx` manquant | passer `registry_tool = "regctl"` (requêtes `HEAD`), ou connecter le registre |
| « conteneur absent » alors qu'il tourne | nom de projet compose différent | vérifier `com.docker.compose.project` (étape 2) |
| « compose config a échoué » | `.env` manquant ou variable obligatoire absente dans `compose_dir` | exécuter la commande à la main pour voir l'erreur (sans afficher la sortie) |
| tests de fumée « INATTENDU » | `smoke_expect` mal calibré, ou le serveur n'atteint pas son propre nom | adapter ; ne pas désactiver sans le dire |
| mise à jour « hors de la fenêtre » en continu | `window` trop étroite pour le rythme du minuteur | le minuteur passe chaque heure : une fenêtre d'une heure suffit |
| « simulation » partout | `--dry-run` actif | retirer l'option |

## 12. Rapport à rendre à l'utilisateur après chaque étape

Pour chaque étape : les commandes lancées (avec celles qui ont **modifié** quelque chose), le résultat observé comparé à l'attendu,
les anomalies et les écarts à cette procédure, et ce que tu proposes ensuite. Aucun secret, aucune IP publique, aucune donnée personnelle.
