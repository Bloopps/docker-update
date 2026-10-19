// Package deploy embarque les unités systemd : `docker-update init` les installe lui-même.
package deploy

import _ "embed"

//go:embed docker-update.service
var Service string

//go:embed docker-update.timer
var Timer string

// UnitVersion est la version des unités fournies (ligne « # docker-update-unit-version: N » en tête de chaque fichier).
// À incrémenter quand une unité change d'une façon que `docker-update migrate` doit proposer d'appliquer.
const UnitVersion = 1
