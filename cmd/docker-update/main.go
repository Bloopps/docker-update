// Commande docker-update : met à jour des images Docker Compose avec un délai de sécurité.
package main

import (
	"fmt"
	"os"
	"runtime"
)

// version est remplacée à la compilation : -ldflags "-X main.version=<git describe>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run exécute la commande et renvoie le code de sortie (0 ok, 1 échec, 2 erreur de configuration).
func run(args []string, stdout, stderr *os.File) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintf(stdout, "docker-update %s %s/%s %s\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return 0
	}
	fmt.Fprintln(stderr, "docker-update : commande non portée pour l'instant (réécriture en cours, voir docs/PORTAGE.md)")
	return 2
}
