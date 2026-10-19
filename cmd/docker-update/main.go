// Commande docker-update : met à jour des images Docker Compose avec un délai de sécurité.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/Bloopps/docker-update/internal/cli"
)

// version est remplacée à la compilation : -ldflags "-X main.version=<git describe>".
var version = "dev"

func versionString() string {
	return fmt.Sprintf("%s %s/%s %s", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

func main() {
	env := cli.DefaultEnv(versionString())
	env.Tag = version
	os.Exit(cli.Main(os.Args[1:], env))
}
