// Package tomlmini lit un sous-ensemble strict de TOML : la bibliothèque standard de Go n'a pas de lecteur TOML
// et le projet n'a aucune dépendance tierce.
//
// Sont pris en charge : commentaires, tables [nom], tableaux de tables [[nom]], clés simples (nues ou entre
// guillemets), chaînes "…" (échappements \b \t \n \f \r \" \\ \uXXXX \UXXXXXXXX) et '…', entiers, booléens,
// tableaux de ces valeurs (sur plusieurs lignes, virgule finale, commentaires). Tout le reste (tables en ligne,
// flottants, dates, chaînes multilignes, clés pointées, tableaux imbriqués) est refusé avec le message
// « TOML non pris en charge : … (ligne N) ».
//
// Le lecteur accepte tout ce que tomllib de Python accepte dans ce sous-ensemble (le fichier de configuration
// d'un utilisateur existant doit se lire sans changement).
package tomlmini
