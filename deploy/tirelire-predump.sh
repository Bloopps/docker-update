#!/bin/bash
# Sauvegarde de la base de la tirelire AVANT une mise à jour (crochet pre_hook de docker-update).
# Installation : /usr/local/sbin/tirelire-predump.sh, propriétaire root, mode 750.
# Code de sortie non nul => docker-update annule la mise à jour sans rien modifier.
set -euo pipefail
umask 077

DIR=/home/ubuntu/tirelire/backups
KEEP=14
CONTAINER=tirelire-db
TS=$(date +%Y%m%d-%H%M%S)
OUT="$DIR/tirelire-avant-maj-$TS.sql.gz"
TMP="$OUT.tmp"
trap 'rm -f -- "$TMP"' EXIT

[ -d "$DIR" ] || { echo "dossier de sauvegarde absent : $DIR" >&2; exit 1; }
docker exec "$CONTAINER" pg_dump -U postgres -d tirelire --no-owner | gzip -9 > "$TMP"

# Contrôles : gzip valide, taille plausible (un schéma vide fait déjà plusieurs Ko).
gzip -t "$TMP"
[ "$(stat -c %s "$TMP")" -gt 2000 ] || { echo "sauvegarde trop petite, abandon" >&2; exit 1; }
mv -- "$TMP" "$OUT"
trap - EXIT

# Garde les $KEEP plus récentes (uniquement celles créées par ce script).
ls -1t -- "$DIR"/tirelire-avant-maj-*.sql.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | xargs -r rm -f --
echo "sauvegarde : $OUT"
