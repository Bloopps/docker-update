// Package deploy embarque les unités systemd : `docker-update init` les installe lui-même.
package deploy

import _ "embed"

//go:embed docker-update.service
var Service string

//go:embed docker-update.timer
var Timer string
