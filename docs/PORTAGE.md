# Portage Python → Go : suivi

Mission de référence : `docs/PROMPT-reecriture-go.md`. Le script `docker-update.py` et ses tests sont la spécification exécutable :
aucun changement de comportement pendant le portage (phases 0 à 6). En cas de doute, **le Python fait foi** : il a été supprimé en phase 6 mais reste lisible dans l'historique git (dernier état : commit `50952f1`, `git show 50952f1:docker-update.py`).

## 1. Version de Go retenue

- Go n'a pas de version LTS. Le propriétaire a demandé « la dernière LTS » : on retient la **dernière version stable publiée**
  (go.dev/dl, 19 octobre 2026) : **Go 1.27.2** (la précédente série stable est 1.26.9).
- Directive de `go.mod` : `go 1.27` (le minimum de la mission est 1.24 ; la CI épingle `1.27.2`). Aucun `require`, ni `go.sum`, ni `vendor/`.
- Le binaire reste statique : `CGO_ENABLED=0`, `-trimpath`, `linux/amd64` et `linux/arm64`.
- Installation dans l'environnement de session : archive officielle `go1.27.2.linux-amd64.tar.gz`, empreinte SHA-256 vérifiée
  (`ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5`).

## 2. Plan par phases

| Phase | Contenu | État |
| --- | --- | --- |
| 0 | `go.mod`, squelette, CI Go, ce document | fait |
| 1 | `textutil` : durées, fenêtres, `CleanText`, versions, `split_ref`, motifs de notes, `compact_*` | fait |
| 2 | `tomlmini` + `config` (clés supprimées, groupes, `examples/`) + fuzz | fait |
| 3 | `state`, `notify` (Notifier, Report, webhook), `github`, `smoke`, `dockerx` | fait |
| 4 | `updater` (evaluate, security, release notes, apply, cleanup, cycle, now, select, status) + `fakeworld` | fait |
| 5 | `cli` + `main`, parité bout en bout (`DU_BIN`), test différentiel | fait |
| 6 | Bascule : suppression du Python, faux `docker` en Go (`testdata/fakedocker`) et tests `e2e/`, docs, mutation manuelle | fait, sauf récapitulatif des propositions et vérification §10 |
| 7 | `init`, `add`, `delete`, `list` | fait |

Ajustements au plan : aucun pour l'instant. Le fuzz court (10 s) du lecteur TOML est dans la CI depuis la phase 2.

## 3. Écarts connus

Écarts de la phase 1 (`internal/textutil`), tous sans effet sur les entrées réelles :

1. **Chiffres ASCII seulement** dans les durées, fenêtres et versions : `\d` de Python accepte aussi les chiffres Unicode (« ٣d »). Go refuse.
2. **Fin de chaîne stricte** : `^…$` de Python accepte un saut de ligne final (`"v1.0\n"` passe `TAG_RE`) ; en Go `$` est strict. À étendre à `NAME_RE`, `REPO_RE`, `GHSA_RE` en phase 2 (voir Proposition 1).
3. **Entiers bornés** : `parse_version` accepte des entiers arbitrairement grands en Python ; une version hors de la plage `int` donne « illisible » en Go (on ne devine jamais).
4. **Dates ISO 8601** : Go lit les formes ISO courantes (`Z`, `+hh:mm`, `+hhmm`, sans fuseau = heure locale, date seule). Python 3.11 en accepte quelques autres (semaines ISO, par exemple), absentes des réponses de GitHub.
5. **Frontières de mot `\b`** : en RE2 elles sont ASCII ; les motifs de notes de release les reproduisent à la main avec la notion Unicode de Python (`éCVE-2026-12345` ne correspond pas), voir `TestWordBoundariesAreUnicode`.
6. **Tables Unicode** : `isprintable()` (Python 3.11) et `unicode.IsPrint` (Go 1.27) viennent de versions différentes d'Unicode ; seuls des caractères récents peuvent différer.

Écarts de la phase 2 (`internal/tomlmini`, `internal/config`) :

7. **Expressions `release_notes_ignore`** : Go compile en RE2 (pas de recherche en avant ni de référence arrière, `\d \w \b` ASCII) alors que
   Python utilise `re` ; une expression valable en Python mais hors RE2 (`(?=x)`) donne une erreur de configuration claire (« syntaxe RE2 : ni
   lookahead ni référence arrière ») et non un comportement différent. Le texte de l'erreur de syntaxe vient de Go, pas de Python.
8. **Entiers TOML sur 64 bits** : `tomllib` lit des entiers arbitraires ; au-delà de 64 bits, « TOML non pris en charge ». Sans effet : aucun réglage n'en demande.
9. **Fichier de configuration illisible** (droits, répertoire) : Python s'arrête sur une trace (code 1) ; Go répond « configuration illisible : … » (code 2).
10. **`settings` / `service` de mauvaise forme** (`service = ["a"]`, `[service]` au lieu de `[[service]]`) : Python se comporte de façon imprévisible ; Go refuse avec un message clair.
11. **Messages TOML** : les erreurs de syntaxe sont celles du lecteur Go (« TOML invalide : … (ligne N) ») ; celles de `tomllib` ne sont pas reproduites mot pour mot (seul le préfixe « configuration illisible : » est commun).

Contrôles de parité de la phase 2 (scripts jetables, non conservés dans le dépôt) : environ 25 000 documents TOML (exemples mutés, cas limites) lus par
`tomllib` et par le lecteur Go : mêmes acceptations, mêmes valeurs, aucun refus de Go sur un TOML pris en charge ; environ 16 000 configurations aléatoires passées à
`parse_config` et à `config.Load` : mêmes acceptations, mêmes valeurs, mêmes messages d'erreur (hors texte de syntaxe d'expression, écart n° 7).

Écarts de la phase 3 (`state`, `notify`, `github`, `smoke`, `dockerx`) :

12. **État** : Go omet les clés vides d'un cache GitHub (`etag` vide, `err` à 0, `full` à 0, `ok` faux) ; Python les écrit parfois (`"etag": ""`). Les deux versions
    lisent l'absence et la valeur vide de la même façon. Pour un état « plein » le fichier réécrit est identique octet pour octet à celui de Python
    (`TestStateWrittenByPythonLoadsAndIsRewrittenIdentically`, fichier produit par `State.save` du Python). Les clés inconnues d'un enregistrement ne sont pas conservées.
13. **Entrée standard des commandes** : `subprocess.run` hérite de l'entrée standard de l'outil ; Go donne une entrée vide à `docker` (elle peut porter le secret de `set-webhook`).
14. **Code de retour d'une commande tuée par un signal** : `-signal` comme Python ; une commande non exécutable (droits) donne 126 avec un message (Python : trace).
15. **Sorties de commandes** : lues en octets puis converties (octets invalides remplacés par U+FFFD, fins de ligne universelles) ; Python en mode texte lèverait une erreur de décodage sur un octet invalide.
16. **Erreurs d'envoi** (« notification non envoyée (…) ») : le type est `HTTPError`, `TimeoutError` ou `URLError` comme en Python ; pour une erreur d'un autre type, Go donne son nom de type Go. Jamais le texte de l'erreur (il contient l'URL).
17. **Webhook ntfy** : l'en-tête `Title` remplace chaque caractère non ASCII par `?` (comme `encode("ascii", "replace")`) ; un titre contenant un saut de ligne ferait refuser la requête par `net/http` (Python lèverait `ValueError`) ; les titres sont fabriqués par l'outil, jamais copiés tels quels depuis l'extérieur.
18. **Délais HTTP** : `http.Client.Timeout` est un délai total (15 s GitHub, 10 s webhook et test de fumée) ; en Python c'est un délai par opération de socket.

Écarts de la phase 4 (`updater`, `fakeworld`) :

19. **Horloge** : l'heure est un entier de secondes (`int64`) ; Python manipule un flottant mais ne s'en sert qu'à travers `int(now())`. Dans les tests, `Clock.days(n)` arrondit à la seconde.
20. **`image:` non textuelle** dans la sortie de `docker compose config` : Python applique `str()` (une image `null` devient « None »), Go applique `fmt.Sprint`. Compose écrit toujours une chaîne.
21. **`RepoDigests` qui n'est pas une liste de chaînes** : les éléments qui ne sont pas des chaînes sont ignorés (Python lèverait une exception sur un entier). Docker renvoie toujours une liste de chaînes.
22. **Blancs et découpage** : `str.strip()` et `str.split()` de Python sont reproduits (`textutil.Strip`, `textutil.Fields` : blancs Unicode plus `\x1c` à `\x1f`), ainsi que `s[:n]` en caractères (`textutil.Truncate`, `textutil.Slice`).
23. **Fichier du jeton GitHub** : lu en octets (un octet UTF-8 invalide ne fait pas lever d'exception comme en Python) ; toujours jamais journalisé.
24. **Erreurs** : `Cycle`, `NowCmd` et `Hold` renvoient `(code, erreur)` au lieu de lever `UpdateError` ; `cli.Main` en fait le même code 1.
25. **POST ntfy** : `urllib` ajoute `Content-Type: application/x-www-form-urlencoded` à tout POST ; Go n'envoie aucun type pour le corps texte de ntfy (sans effet côté serveur). Le test différentiel ignore cet en-tête.
26. **Commande `version`** : nouvelle (le Python n'en a pas, et la refuse) ; elle affiche `docker-update <version> GOOS/GOARCH goX`, la version étant injectée par `-ldflags -X main.version`.

## 4. Inventaire des tests : Python → Go

Un test Python = une ligne. Colonne « Go » : nom prévu (paquet, test ou sous-test) ; l'état passe à « fait » quand le test Go existe et passe.
Les classes de test héritant de `Base` ou de `E2E` sont comptées une fois ici ; l'héritage multiplie les exécutions (269 au total pour le Python).

| Fichier | Classe | Test Python | Phase | Paquet Go prévu | État |
| --- | --- | --- | --- | --- | --- |
| test_docker_update.py | TestHelpers | `test_parse_duration` | 1 | internal/textutil : `TestParseDuration (+ TestFmtDuration)` | fait |
| test_docker_update.py | TestHelpers | `test_split_ref` | 1 | internal/textutil : `TestSplitRef` | fait |
| test_docker_update.py | TestHelpers | `test_windows` | 1 | internal/textutil : `TestWindows` | fait |
| test_docker_update.py | TestConfig | `test_valid_and_defaults` | 2 | internal/config : `TestValidAndDefaults` | fait |
| test_docker_update.py | TestConfig | `test_rejections` | 2 | internal/config : `TestRejections (+ TestRemovedKeys, TestMessagesAreThoseOfThePython)` | fait |
| test_docker_update.py | TestConfig | `test_duplicates_and_groups` | 2 | internal/config : `TestDuplicatesAndGroups` | fait |
| test_docker_update.py | TestConfig | `test_example_config_is_valid` | 2 | internal/config : `TestExampleConfigIsValid` | fait |
| test_docker_update.py | TestConfig | `test_load_config_errors` | 2 | internal/config : `TestLoadConfigErrors` | fait |
| test_docker_update.py | TestConfig | `test_trusted_file` | 2 | internal/config : `TestTrustedFile` | fait |
| test_docker_update.py | TestCycle | `test_up_to_date_does_nothing` | 4 | internal/updater : `TestCycle_UpToDateDoesNothing` | fait |
| test_docker_update.py | TestCycle | `test_new_digest_is_deferred_then_applied` | 4 | internal/updater : `TestCycle_NewDigestIsDeferredThenApplied` | fait |
| test_docker_update.py | TestCycle | `test_order_of_operations` | 4 | internal/updater : `TestCycle_OrderOfOperations` | fait |
| test_docker_update.py | TestCycle | `test_digest_moving_resets_timer` | 4 | internal/updater : `TestCycle_DigestMovingResetsTimer` | fait |
| test_docker_update.py | TestCycle | `test_tag_moving_back_clears_state` | 4 | internal/updater : `TestCycle_TagMovingBackClearsState` | fait |
| test_docker_update.py | TestCycle | `test_state_persists_between_runs` | 4 | internal/updater : `TestCycle_StatePersistsBetweenRuns` | fait |
| test_docker_update.py | TestCycle | `test_check_never_applies_or_notifies` | 4 | internal/updater : `TestCycle_CheckNeverAppliesOrNotifies` | fait |
| test_docker_update.py | TestFailures | `test_failed_start_alerts_and_never_rolls_back` | 4 | internal/updater : `TestFailures_FailedStartAlertsAndNeverRollsBack` | fait |
| test_docker_update.py | TestFailures | `test_failed_digest_is_not_retried_but_new_one_is` | 4 | internal/updater : `TestFailures_FailedDigestIsNotRetriedButNewOneIs` | fait |
| test_docker_update.py | TestFailures | `test_restarting_after_up_counts_as_failure` | 4 | internal/updater : `TestFailures_RestartingAfterUpCountsAsFailure` | fait |
| test_docker_update.py | TestFailures | `test_pull_failure_aborts_before_any_change` | 4 | internal/updater : `TestFailures_PullFailureAbortsBeforeAnyChange` | fait |
| test_docker_update.py | TestFailures | `test_registry_error_is_reported_once` | 4 | internal/updater : `TestFailures_RegistryErrorIsReportedOnce` | fait |
| test_docker_update.py | TestFailures | `test_smoke_test` | 4 | internal/updater : `TestFailures_SmokeTest` | fait |
| test_docker_update.py | TestFailures | `test_smoke_test_passes` | 4 | internal/updater : `TestFailures_SmokeTestPasses` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_notify_mode_notifies_once_and_never_applies` | 4 | internal/updater : `TestModes_NotifyModeNotifiesOnceAndNeverApplies` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_manual_mode_is_silent` | 4 | internal/updater : `TestModes_ManualModeIsSilent` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_window` | 4 | internal/updater : `TestModes_Window` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_global_window_and_override` | 4 | internal/updater : `TestModes_GlobalWindowAndOverride` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_hold` | 4 | internal/updater : `TestModes_Hold` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_hold_survives_restart` | 4 | internal/updater : `TestModes_HoldSurvivesRestart` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_skips` | 4 | internal/updater : `TestModes_Skips` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_floating_tag_warning` | 4 | internal/updater : `TestModes_FloatingTagWarning` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_regctl_backend` | 4 | internal/updater : `TestModes_RegctlBackend` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_bad_manifest_output_is_an_error` | 4 | internal/updater : `TestModes_BadManifestOutputIsAnError` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_regctl_bad_output_is_an_error` | 4 | internal/updater : `TestModes_RegctlBadOutputIsAnError` | fait |
| test_docker_update.py | TestModesAndPolicies | `test_buildx_reads_the_raw_manifest_and_not_a_template` | 4 | internal/updater : `TestModes_BuildxReadsTheRawManifestAndNotATemplate` | fait |
| test_docker_update.py | TestGroups | `test_group_updates_in_order_with_single_pull` | 4 | internal/updater : `TestGroups_UpdatesInOrderWithSinglePull` | fait |
| test_docker_update.py | TestGroups | `test_group_failure_stops_there_and_leaves_everything_as_is` | 4 | internal/updater : `TestGroups_FailureStopsThereAndLeavesEverythingAsIs` | fait |
| test_docker_update.py | TestGroups | `test_group_stops_at_the_first_failure_and_names_the_untouched` | 4 | internal/updater : `TestGroups_StopsAtTheFirstFailureAndNamesTheUntouched` | fait |
| test_docker_update.py | TestGroups | `test_shared_ref_keeps_timer_when_only_one_member_is_up_to_date` | 4 | internal/updater : `TestGroups_SharedRefKeepsTimerWhenOnlyOneMemberIsUpToDate` | fait |
| test_docker_update.py | TestGroups | `test_now_targets_group_by_name` | 4 | internal/updater : `TestGroups_NowTargetsGroupByName` | fait |
| test_docker_update.py | TestNow | `test_now_bypasses_cooldown_window_and_mode` | 4 | internal/updater : `TestNow_BypassesCooldownWindowAndMode` | fait |
| test_docker_update.py | TestNow | `test_now_unknown_name` | 4 | internal/updater : `TestNow_UnknownName` | fait |
| test_docker_update.py | TestNow | `test_now_nothing_to_do` | 4 | internal/updater : `TestNow_NothingToDo` | fait |
| test_docker_update.py | TestNow | `test_now_still_retries_a_failed_digest` | 4 | internal/updater : `TestNow_StillRetriesAFailedDigest` | fait |
| test_docker_update.py | TestDryRun | `test_dry_run_changes_nothing` | 4 | internal/updater : `TestDryRun_ChangesNothing` | fait |
| test_docker_update.py | TestDryRun | `test_real_runner_dry_run_only_prints` | 3 | internal/dockerx : `TestRunnerDryRunOnlyPrints` | fait |
| test_docker_update.py | TestInfra | `test_runner` | 3 | internal/dockerx : `TestRunner` (+ `TestRunnerTextUsesUniversalNewlines`) | fait |
| test_docker_update.py | TestInfra | `test_runner_bytes_keeps_exact_bytes` | 3 | internal/dockerx : `TestRunnerBytesKeepsExactBytes` | fait |
| test_docker_update.py | TestInfra | `test_state_roundtrip_and_corruption` | 3 | internal/state : `TestRoundtripAndCorruption` (+ `TestStateWrittenByPythonLoadsAndIsRewrittenIdentically`) | fait |
| test_docker_update.py | TestInfra | `test_notifier_discord_payload_and_secret_never_logged` | 3 | internal/notify : `TestNotifierDiscordPayloadAndSecretNeverLogged` | fait |
| test_docker_update.py | TestInfra | `test_notifier_without_url_and_other_formats` | 3 | internal/notify : `TestNotifierWithoutURLAndOtherFormats` | fait |
| test_docker_update.py | TestInfra | `test_http_status_unreachable` | 3 | internal/smoke : `TestStatusUnreachable` | fait |
| test_docker_update.py | TestInfra | `test_validate` | 5 | internal/cli : `TestValidate` | fait |
| test_docker_update.py | TestSecurityProperties | `test_compose_config_content_is_never_logged` | 4 | internal/updater : `TestSecurityProperties_ComposeConfigContentIsNeverLogged` | fait |
| test_docker_update.py | TestSecurityProperties | `test_commands_never_use_a_shell` | 4 | internal/updater : `TestSecurityProperties_CommandsNeverUseAShell` | fait |
| test_docker_update.py | TestSecurityProperties | `test_pull_is_by_digest_of_the_evaluated_image` | 4 | internal/updater : `TestSecurityProperties_PullIsByDigestOfTheEvaluatedImage` | fait |
| test_docker_update.py | TestVersions | `test_parse_version` | 1 | internal/textutil : `TestParseVersion` | fait |
| test_docker_update.py | TestVersions | `test_ranges` | 1 | internal/textutil : `TestRanges` | fait |
| test_docker_update.py | TestVersions | `test_unreadable_never_guesses` | 1 | internal/textutil : `TestUnreadableNeverGuesses` | fait |
| test_docker_update.py | TestVersions | `test_clean_text` | 1 | internal/textutil : `TestCleanText` | fait |
| test_docker_update.py | TestVersions | `test_compact_advisory` | 1 | internal/textutil : `TestCompactAdvisory` | fait |
| test_docker_update.py | TestVersions | `test_config_security_keys` | 2 | internal/config : `TestConfigSecurityKeys` | fait |
| test_docker_update.py | TestSecurity | `test_fast_track_waits_security_cooldown_then_applies` | 4 | internal/updater : `TestSecurity_FastTrackWaitsSecurityCooldownThenApplies` | fait |
| test_docker_update.py | TestSecurity | `test_clock_starts_at_the_latest_of_first_seen_and_publication` | 4 | internal/updater : `TestSecurity_ClockStartsAtTheLatestOfFirstSeenAndPublication` | fait |
| test_docker_update.py | TestSecurity | `test_a_brand_new_advisory_is_not_instant` | 4 | internal/updater : `TestSecurity_ABrandNewAdvisoryIsNotInstant` | fait |
| test_docker_update.py | TestSecurity | `test_candidate_still_vulnerable_keeps_normal_delay` | 4 | internal/updater : `TestSecurity_CandidateStillVulnerableKeepsNormalDelay` | fait |
| test_docker_update.py | TestSecurity | `test_candidate_version_unreadable_is_alert_only` | 4 | internal/updater : `TestSecurity_CandidateVersionUnreadableIsAlertOnly` | fait |
| test_docker_update.py | TestSecurity | `test_alert_only_policy` | 4 | internal/updater : `TestSecurity_AlertOnlyPolicy` | fait |
| test_docker_update.py | TestSecurity | `test_severity_and_lookback_filters` | 4 | internal/updater : `TestSecurity_SeverityAndLookbackFilters` | fait |
| test_docker_update.py | TestSecurity | `test_withdrawn_advisory_is_ignored` | 4 | internal/updater : `TestSecurity_WithdrawnAdvisoryIsIgnored` | fait |
| test_docker_update.py | TestSecurity | `test_unaffected_current_version_is_ignored` | 4 | internal/updater : `TestSecurity_UnaffectedCurrentVersionIsIgnored` | fait |
| test_docker_update.py | TestSecurity | `test_critical_ignores_window_but_high_does_not` | 4 | internal/updater : `TestSecurity_CriticalIgnoresWindowButHighDoesNot` | fait |
| test_docker_update.py | TestSecurity | `test_up_to_date_but_vulnerable_alerts_once` | 4 | internal/updater : `TestSecurity_UpToDateButVulnerableAlertsOnce` | fait |
| test_docker_update.py | TestSecurity | `test_unknown_current_version_alerts_once` | 4 | internal/updater : `TestSecurity_UnknownCurrentVersionAlertsOnce` | fait |
| test_docker_update.py | TestSecurity | `test_unreadable_range_is_alert_only` | 4 | internal/updater : `TestSecurity_UnreadableRangeIsAlertOnly` | fait |
| test_docker_update.py | TestSecurity | `test_check_never_pulls_or_notifies` | 4 | internal/updater : `TestSecurity_CheckNeverPullsOrNotifies` | fait |
| test_docker_update.py | TestSecurity | `test_dry_run_neither_pulls_nor_persists` | 4 | internal/updater : `TestSecurity_DryRunNeitherPullsNorPersists` | fait |
| test_docker_update.py | TestSecurity | `test_failed_fast_track_is_not_retried` | 4 | internal/updater : `TestSecurity_FailedFastTrackIsNotRetried` | fait |
| test_docker_update.py | TestSecurity | `test_github_outage_does_not_block_normal_updates` | 4 | internal/updater : `TestSecurity_GithubOutageDoesNotBlockNormalUpdates` | fait |
| test_docker_update.py | TestSecurity | `test_rate_limit_returns_old_items` | 4 | internal/updater : `TestSecurity_RateLimitReturnsOldItems` | fait |
| test_docker_update.py | TestSecurity | `test_poll_interval_and_etag` | 4 | internal/updater : `TestSecurity_PollIntervalAndEtag` | fait |
| test_docker_update.py | TestSecurity | `test_token_is_sent_but_never_logged` | 4 | internal/updater : `TestSecurity_TokenIsSentButNeverLogged` | fait |
| test_docker_update.py | TestSecurity | `test_untrusted_summary_cannot_inject` | 4 | internal/updater : `TestSecurity_UntrustedSummaryCannotInject` | fait |
| test_docker_update.py | TestSecurity | `test_old_alerts_are_pruned` | 4 | internal/updater : `TestSecurity_OldAlertsArePruned` | fait |
| test_docker_update.py | TestSecurity | `test_publication_date_delays_a_late_advisory` | 4 | internal/updater : `TestSecurity_PublicationDateDelaysALateAdvisory` | fait |
| test_docker_update.py | TestSecurity | `test_unreadable_second_range_blocks_acceleration` | 4 | internal/updater : `TestSecurity_UnreadableSecondRangeBlocksAcceleration` | fait |
| test_docker_update.py | TestSecurity | `test_check_reports_detected_version_or_its_absence` | 4 | internal/updater : `TestSecurity_CheckReportsDetectedVersionOrItsAbsence` | fait |
| test_docker_update.py | TestSecurity | `test_token_not_logged_when_the_api_fails` | 4 | internal/updater : `TestSecurity_TokenNotLoggedWhenTheAPIFails` | fait |
| test_docker_update.py | TestSecurity | `test_notify_mode_service_gets_an_alert_not_an_acceleration` | 4 | internal/updater : `TestSecurity_NotifyModeServiceGetsAnAlertNotAnAcceleration` | fait |
| test_docker_update.py | TestSecurity | `test_group_gets_one_accelerated_update` | 4 | internal/updater : `TestSecurity_GroupGetsOneAcceleratedUpdate` | fait |
| test_docker_update.py | TestGithubGet | `test_ok_headers_and_etag` | 3 | internal/github : `TestGetOKHeadersAndEtag` | fait |
| test_docker_update.py | TestGithubGet | `test_token_and_conditional_request` | 3 | internal/github : `TestGetTokenAndConditionalRequest` | fait |
| test_docker_update.py | TestGithubGet | `test_errors` | 3 | internal/github : `TestGetErrors` | fait |
| test_docker_update.py | TestMain | `test_check_and_run_pass_their_names_and_options_on` | 5 | internal/cli : `TestCheckAndRunPassTheirNamesAndOptionsOn` | fait |
| test_docker_update.py | TestMain | `test_validate_and_status_and_hold` | 5 | internal/cli : `TestValidateAndStatusAndHold` | fait |
| test_docker_update.py | TestMain | `test_notify_test_command` | 5 | internal/cli : `TestNotifyTestCommand` | fait |
| test_docker_update.py | TestMain | `test_bad_config_exit_code_2` | 5 | internal/cli : `TestBadConfigExitCode2` | fait |
| test_docker_update.py | TestMain | `test_lock_prevents_concurrent_runs` | 5 | internal/cli : `TestLockPreventsConcurrentRuns` | fait |
| test_docker_update.py | TestMain | `test_untrusted_config_refused_as_root` | 5 | internal/cli : `TestUntrustedConfigRefusedAsRoot` | fait |
| test_docker_update.py | TestReleaseScan | `test_parse_release_version` | 1 | internal/textutil : `TestParseReleaseVersion` | fait |
| test_docker_update.py | TestReleaseScan | `test_strong_patterns` | 1 | internal/textutil : `TestStrongPatterns` | fait |
| test_docker_update.py | TestReleaseScan | `test_near_misses_do_not_match` | 1 | internal/textutil : `TestNearMissesDoNotMatch (+ TestWordBoundariesAreUnicode)` | fait |
| test_docker_update.py | TestReleaseScan | `test_security_alone_is_weak` | 1 | internal/textutil : `TestSecurityAloneIsWeak` | fait |
| test_docker_update.py | TestReleaseScan | `test_hits_are_deduplicated_and_bounded` | 1 | internal/textutil : `TestHitsAreDeduplicatedAndBounded` | fait |
| test_docker_update.py | TestReleaseScan | `test_only_the_head_of_a_huge_note_is_read` | 1 | internal/textutil : `TestOnlyTheHeadOfAHugeNoteIsRead` | fait |
| test_docker_update.py | TestReleaseScan | `test_compact_release` | 1 | internal/textutil : `TestCompactRelease` | fait |
| test_docker_update.py | TestReleaseScan | `test_config_release_keys` | 2 | internal/config : `TestConfigReleaseKeys` | fait |
| test_docker_update.py | TestReleaseScan | `test_example_configs_are_valid` | 2 | internal/config : `TestExampleConfigIsValid` | fait |
| test_docker_update.py | TestReleaseScan | `test_watchtower_migration_example` | 2 | internal/config : `TestWatchtowerMigrationExample` | fait |
| test_docker_update.py | TestReleaseNotes | `test_real_world_cases` | 4 | internal/updater : `TestReleaseNotes_RealWorldCases` | fait |
| test_docker_update.py | TestReleaseNotes | `test_motifs_are_listed` | 4 | internal/updater : `TestReleaseNotes_MotifsAreListed` | fait |
| test_docker_update.py | TestReleaseNotes | `test_no_releases_means_no_alert_and_no_error` | 4 | internal/updater : `TestReleaseNotes_NoReleasesMeansNoAlertAndNoError` | fait |
| test_docker_update.py | TestReleaseNotes | `test_older_or_equal_releases_are_ignored` | 4 | internal/updater : `TestReleaseNotes_OlderOrEqualReleasesAreIgnored` | fait |
| test_docker_update.py | TestReleaseNotes | `test_one_alert_per_service_and_tag_and_persisted` | 4 | internal/updater : `TestReleaseNotes_OneAlertPerServiceAndTagAndPersisted` | fait |
| test_docker_update.py | TestReleaseNotes | `test_dedup_key_is_per_service` | 4 | internal/updater : `TestReleaseNotes_DedupKeyIsPerService` | fait |
| test_docker_update.py | TestReleaseNotes | `test_drafts_and_prereleases_are_ignored` | 4 | internal/updater : `TestReleaseNotes_DraftsAndPrereleasesAreIgnored` | fait |
| test_docker_update.py | TestReleaseNotes | `test_unreadable_release_tags_are_skipped` | 4 | internal/updater : `TestReleaseNotes_UnreadableReleaseTagsAreSkipped` | fait |
| test_docker_update.py | TestReleaseNotes | `test_unreadable_current_version_alerts_once_and_scans_nothing` | 4 | internal/updater : `TestReleaseNotes_UnreadableCurrentVersionAlertsOnceAndScansNothing` | fait |
| test_docker_update.py | TestReleaseNotes | `test_never_accelerates_or_applies` | 4 | internal/updater : `TestReleaseNotes_NeverAcceleratesOrApplies` | fait |
| test_docker_update.py | TestReleaseNotes | `test_ignore_patterns` | 4 | internal/updater : `TestReleaseNotes_IgnorePatterns` | fait |
| test_docker_update.py | TestReleaseNotes | `test_ignore_is_case_insensitive_and_hides_only_matching_hits` | 4 | internal/updater : `TestReleaseNotes_IgnoreIsCaseInsensitiveAndHidesOnlyMatchingHits` | fait |
| test_docker_update.py | TestReleaseNotes | `test_ignore_can_silence_the_weak_signal_too` | 4 | internal/updater : `TestReleaseNotes_IgnoreCanSilenceTheWeakSignalToo` | fait |
| test_docker_update.py | TestReleaseNotes | `test_strong_release_has_no_extra_info_alert` | 4 | internal/updater : `TestReleaseNotes_StrongReleaseHasNoExtraInfoAlert` | fait |
| test_docker_update.py | TestReleaseNotes | `test_weak_signals_are_grouped_and_deduplicated` | 4 | internal/updater : `TestReleaseNotes_WeakSignalsAreGroupedAndDeduplicated` | fait |
| test_docker_update.py | TestReleaseNotes | `test_a_release_edited_from_weak_to_strong_is_alerted` | 4 | internal/updater : `TestReleaseNotes_AReleaseEditedFromWeakToStrongIsAlerted` | fait |
| test_docker_update.py | TestReleaseNotes | `test_untrusted_notes_cannot_inject` | 4 | internal/updater : `TestReleaseNotes_UntrustedNotesCannotInject` | fait |
| test_docker_update.py | TestReleaseNotes | `test_check_logs_but_sends_nothing` | 4 | internal/updater : `TestReleaseNotes_CheckLogsButSendsNothing` | fait |
| test_docker_update.py | TestReleaseNotes | `test_api_failure_never_blocks_normal_updates` | 4 | internal/updater : `TestReleaseNotes_APIFailureNeverBlocksNormalUpdates` | fait |
| test_docker_update.py | TestReleaseNotes | `test_rate_limit_waits_the_whole_interval_and_keeps_old_items` | 4 | internal/updater : `TestReleaseNotes_RateLimitWaitsTheWholeIntervalAndKeepsOldItems` | fait |
| test_docker_update.py | TestReleaseNotes | `test_history_once_then_only_the_latest_releases` | 4 | internal/updater : `TestReleaseNotes_HistoryOnceThenOnlyTheLatestReleases` | fait |
| test_docker_update.py | TestReleaseNotes | `test_a_failed_first_read_is_retried_with_the_full_history` | 4 | internal/updater : `TestReleaseNotes_AFailedFirstReadIsRetriedWithTheFullHistory` | fait |
| test_docker_update.py | TestReleaseNotes | `test_first_ever_rate_limit_also_waits_the_interval` | 4 | internal/updater : `TestReleaseNotes_FirstEverRateLimitAlsoWaitsTheInterval` | fait |
| test_docker_update.py | TestReleaseNotes | `test_same_tag_in_two_repositories_is_alerted_once` | 4 | internal/updater : `TestReleaseNotes_SameTagInTwoRepositoriesIsAlertedOnce` | fait |
| test_docker_update.py | TestReleaseNotes | `test_404_is_cached_for_the_poll_interval` | 4 | internal/updater : `TestReleaseNotes_404IsCachedForThePollInterval` | fait |
| test_docker_update.py | TestReleaseNotes | `test_poll_interval_etag_url_and_shared_cache` | 4 | internal/updater : `TestReleaseNotes_PollIntervalEtagURLAndSharedCache` | fait |
| test_docker_update.py | TestReleaseNotes | `test_repo_defaults_to_advisories` | 4 | internal/updater : `TestReleaseNotes_RepoDefaultsToAdvisories` | fait |
| test_docker_update.py | TestReleaseNotes | `test_release_notes_are_off_by_default` | 4 | internal/updater : `TestReleaseNotes_AreOffByDefault` | fait |
| test_docker_update.py | TestReleaseNotes | `test_token_is_sent_but_never_logged` | 4 | internal/updater : `TestReleaseNotes_TokenIsSentButNeverLogged` | fait |
| test_docker_update.py | TestReleaseNotes | `test_state_keeps_only_compact_data` | 4 | internal/updater : `TestReleaseNotes_StateKeepsOnlyCompactData` | fait |
| test_docker_update.py | TestReleaseNotes | `test_notify_mode_and_unknown_status_services` | 4 | internal/updater : `TestReleaseNotes_NotifyModeAndUnknownStatusServices` | fait |
| test_docker_update.py | TestReleaseNotes | `test_absent_container_is_skipped` | 4 | internal/updater : `TestReleaseNotes_AbsentContainerIsSkipped` | fait |
| test_docker_update.py | TestReleaseNotes | `test_simulation_changes_nothing` | 4 | internal/updater : `TestReleaseNotes_SimulationChangesNothing` | fait |
| test_docker_update.py | TestReleaseBodyLimit | `test_releases_endpoint_accepts_large_bodies` | 3 | internal/github : `TestReleasesEndpointAcceptsLargeBodies` | fait |
| test_docker_update.py | TestCleanup | `test_previous_image_is_removed_after_a_successful_update` | 4 | internal/updater : `TestCleanup_PreviousImageIsRemovedAfterASuccessfulUpdate` | fait |
| test_docker_update.py | TestCleanup | `test_nothing_is_removed_when_the_update_fails` | 4 | internal/updater : `TestCleanup_NothingIsRemovedWhenTheUpdateFails` | fait |
| test_docker_update.py | TestCleanup | `test_nothing_is_removed_without_an_update` | 4 | internal/updater : `TestCleanup_NothingIsRemovedWithoutAnUpdate` | fait |
| test_docker_update.py | TestCleanup | `test_a_refused_removal_does_not_fail_the_update` | 4 | internal/updater : `TestCleanup_ARefusedRemovalDoesNotFailTheUpdate` | fait |
| test_docker_update.py | TestCleanup | `test_cleanup_can_be_disabled_globally_and_per_service` | 4 | internal/updater : `TestCleanup_CanBeDisabledGloballyAndPerService` | fait |
| test_docker_update.py | TestCleanup | `test_default_is_on` | 4 | internal/updater : `TestCleanup_DefaultIsOn` | fait |
| test_docker_update.py | TestCleanup | `test_group_cleans_each_previous_image_once` | 4 | internal/updater : `TestCleanup_GroupCleansEachPreviousImageOnce` | fait |
| test_docker_update.py | TestCleanup | `test_dry_run_removes_nothing` | 4 | internal/updater : `TestCleanup_DryRunRemovesNothing` | fait |
| test_docker_update.py | TestCleanup | `test_now_also_cleans` | 4 | internal/updater : `TestCleanup_NowAlsoCleans` | fait |
| test_docker_update.py | TestCleanup | `test_option_like_image_id_is_never_passed` | 4 | internal/updater : `TestCleanup_OptionLikeImageIDIsNeverPassed` | fait |
| test_docker_update.py | TestStoppedContainers | `test_a_stopped_service_is_not_touched_at_all` | 4 | internal/updater : `TestStopped_AStoppedServiceIsNotTouchedAtAll` | fait |
| test_docker_update.py | TestStoppedContainers | `test_notify_manual_and_check_leave_it_alone_too` | 4 | internal/updater : `TestStopped_NotifyManualAndCheckLeaveItAloneToo` | fait |
| test_docker_update.py | TestStoppedContainers | `test_now_does_not_start_or_update_a_stopped_service` | 4 | internal/updater : `TestStopped_NowDoesNotStartOrUpdateAStoppedService` | fait |
| test_docker_update.py | TestStoppedContainers | `test_restarting_is_not_stopped` | 4 | internal/updater : `TestStopped_RestartingIsNotStopped` | fait |
| test_docker_update.py | TestStoppedContainers | `test_a_started_service_is_picked_up_again` | 4 | internal/updater : `TestStopped_AStartedServiceIsPickedUpAgain` | fait |
| test_docker_update.py | TestStoppedContainers | `test_only_the_stopped_member_of_a_group_is_left_alone` | 4 | internal/updater : `TestStopped_OnlyTheStoppedMemberOfAGroupIsLeftAlone` | fait |
| test_docker_update.py | TestStoppedContainers | `test_unreadable_status_is_an_error_and_changes_nothing` | 4 | internal/updater : `TestStopped_UnreadableStatusIsAnErrorAndChangesNothing` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_the_release_that_cites_the_advisory_completes_it_instead_of_a_second_alert` | 4 | internal/updater : `TestTogether_TheReleaseThatCitesTheAdvisoryCompletesItInsteadOfASecondAlert` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_cve_and_case_are_matched` | 4 | internal/updater : `TestTogether_CVEAndCaseAreMatched` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_a_release_with_more_than_the_advisory_still_alerts_for_the_rest` | 4 | internal/updater : `TestTogether_AReleaseWithMoreThanTheAdvisoryStillAlertsForTheRest` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_weak_signal_in_a_fully_covered_release_is_not_reported` | 4 | internal/updater : `TestTogether_WeakSignalInAFullyCoveredReleaseIsNotReported` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_an_advisory_the_tool_ignores_does_not_cover_the_release` | 4 | internal/updater : `TestTogether_AnAdvisoryTheToolIgnoresDoesNotCoverTheRelease` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_an_advisory_of_another_repository_does_not_cover` | 4 | internal/updater : `TestTogether_AnAdvisoryOfAnotherRepositoryDoesNotCover` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_an_advisory_that_does_not_concern_the_running_version_stays_silent_with_its_release` | 4 | internal/updater : `TestTogether_AnAdvisoryThatDoesNotConcernTheRunningVersionStaysSilentWithItsRelease` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_confirmed_fix_alert_carries_the_release_link_and_acceleration_stays_with_the_advisory` | 4 | internal/updater : `TestTogether_ConfirmedFixAlertCarriesTheReleaseLinkAndAccelerationStaysWithTheAdvisory` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_release_alone_accelerates_nothing_even_when_the_advisory_exists_elsewhere` | 4 | internal/updater : `TestTogether_ReleaseAloneAcceleratesNothingEvenWhenTheAdvisoryExistsElsewhere` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_nothing_is_repeated_next_time` | 4 | internal/updater : `TestTogether_NothingIsRepeatedNextTime` | fait |
| test_docker_update.py | TestAdvisoriesAndReleaseNotesTogether | `test_check_logs_both_and_sends_nothing` | 4 | internal/updater : `TestTogether_CheckLogsBothAndSendsNothing` | fait |
| test_docker_update.py | TestReport | `test_empty` | 3 | internal/notify : `TestReportEmpty` | fait |
| test_docker_update.py | TestReport | `test_title_counts_with_plurals` | 3 | internal/notify : `TestReportTitleCountsWithPlurals` | fait |
| test_docker_update.py | TestReport | `test_level_is_the_worst_one` | 3 | internal/notify : `TestReportLevelIsTheWorstOne` | fait |
| test_docker_update.py | TestReport | `test_sections_come_in_order_of_importance` | 3 | internal/notify : `TestReportSectionsComeInOrderOfImportance` | fait |
| test_docker_update.py | TestReport | `test_ready_versions_are_a_table` | 3 | internal/notify : `TestReportReadyVersionsAreATable` | fait |
| test_docker_update.py | TestReport | `test_an_item_shows_its_first_lines_only_and_bounded` | 3 | internal/notify : `TestReportAnItemShowsItsFirstLinesOnlyAndBounded` | fait |
| test_docker_update.py | TestReport | `test_no_mention_and_no_control_characters` | 3 | internal/notify : `TestReportNoMentionAndNoControlCharacters` | fait |
| test_docker_update.py | TestReport | `test_the_budget_is_respected_and_the_important_part_survives` | 3 | internal/notify : `TestReportBudgetIsRespectedAndTheImportantPartSurvives` | fait |
| test_docker_update.py | TestReport | `test_a_huge_table_is_cut_by_rows_not_by_characters` | 3 | internal/notify : `TestReportAHugeTableIsCutByRowsNotByCharacters` | fait |
| test_docker_update.py | TestReportDelivery | `test_one_message_for_the_whole_pass` | 4 | internal/updater : `TestReportDelivery_OneMessageForTheWholePass` | fait |
| test_docker_update.py | TestReportDelivery | `test_security_alerts_and_updates_share_the_same_message` | 4 | internal/updater : `TestReportDelivery_SecurityAlertsAndUpdatesShareTheSameMessage` | fait |
| test_docker_update.py | TestReportDelivery | `test_nothing_new_means_no_message` | 4 | internal/updater : `TestReportDelivery_NothingNewMeansNoMessage` | fait |
| test_docker_update.py | TestReportDelivery | `test_all_up_to_date_means_no_message` | 4 | internal/updater : `TestReportDelivery_AllUpToDateMeansNoMessage` | fait |
| test_docker_update.py | TestReportDelivery | `test_check_sends_nothing` | 4 | internal/updater : `TestReportDelivery_CheckSendsNothing` | fait |
| test_docker_update.py | TestReportDelivery | `test_now_sends_its_own_single_report` | 4 | internal/updater : `TestReportDelivery_NowSendsItsOwnSingleReport` | fait |
| test_docker_update.py | TestReportDelivery | `test_report_level_is_the_worst_event` | 4 | internal/updater : `TestReportDelivery_ReportLevelIsTheWorstEvent` | fait |
| test_docker_update.py | TestReportDelivery | `test_ntfy_and_json_formats_get_the_same_single_report` | 4 | internal/updater : `TestReportDelivery_NtfyAndJSONFormatsGetTheSameSingleReport` | fait |
| test_docker_update.py | TestReportDelivery | `test_reports_do_not_pile_up_between_passes` | 4 | internal/updater : `TestReportDelivery_ReportsDoNotPileUpBetweenPasses` | fait |
| test_docker_update.py | TestReportDelivery | `test_notification_failure_does_not_break_the_pass` | 4 | internal/updater : `TestReportDelivery_NotificationFailureDoesNotBreakThePass` | fait |
| test_docker_update.py | TestSetWebhook | `test_url_is_written_with_600_and_never_printed` | 3 | internal/notify : `TestSetWebhookURLIsWrittenWith600AndNeverPrinted` | fait |
| test_docker_update.py | TestSetWebhook | `test_the_new_url_is_the_one_used_for_notifications` | 3 | internal/notify : `TestSetWebhookURLIsWrittenWith600AndNeverPrinted` | fait |
| test_docker_update.py | TestSetWebhook | `test_an_existing_file_is_replaced_and_its_rights_fixed` | 3 | internal/notify : `TestSetWebhookAnExistingFileIsReplacedAndItsRightsFixed` | fait |
| test_docker_update.py | TestSetWebhook | `test_invalid_urls_are_refused_without_echoing_them` | 3 | internal/notify : `TestSetWebhookInvalidURLsAreRefusedWithoutEchoingThem` | fait |
| test_docker_update.py | TestSetWebhook | `test_discord_variants_are_accepted` | 3 | internal/notify : `TestSetWebhookDiscordVariantsAreAccepted` | fait |
| test_docker_update.py | TestSetWebhook | `test_only_the_first_line_is_read` | 3 | internal/notify : `TestSetWebhookOnlyTheFirstLineIsRead` | fait |
| test_docker_update.py | TestSetWebhook | `test_other_formats_accept_any_http_url` | 3 | internal/notify : `TestSetWebhookOtherFormatsAcceptAnyHTTPURL` | fait |
| test_docker_update.py | TestSetWebhook | `test_a_target_file_is_required` | 3 | internal/notify : `TestSetWebhookATargetFileIsRequired` | fait |
| test_docker_update.py | TestSetWebhook | `test_dry_run_writes_nothing` | 3 | internal/notify : `TestSetWebhookDryRunWritesNothing` | fait |
| test_docker_update.py | TestSetWebhook | `test_interactive_input_is_hidden` | 5 | internal/cli : `TestInteractiveInputIsHidden` | fait |
| test_docker_update.py | TestSetWebhook | `test_the_url_is_never_a_command_line_argument` | 5 | internal/cli : `TestTheURLIsNeverACommandLineArgument` | fait |
| test_docker_update.py | TestSetWebhook | `test_removed_keys_are_explained` | 2 | internal/config : `TestRemovedKeys` | fait |
| test_docker_update.py | TestManualScan | `test_check_on_a_selection_scans_only_those` | 4 et 5 | internal/updater, cli : `TestManualScan_CheckOnASelectionScansOnlyThose` | fait |
| test_docker_update.py | TestManualScan | `test_no_name_scans_everything` | 4 et 5 | internal/updater, cli : `TestManualScan_NoNameScansEverything` | fait |
| test_docker_update.py | TestManualScan | `test_a_group_name_selects_all_its_members` | 4 et 5 | internal/updater, cli : `TestManualScan_AGroupNameSelectsAllItsMembers` | fait |
| test_docker_update.py | TestManualScan | `test_several_names_keep_the_file_order_and_unknown_is_refused` | 4 et 5 | internal/updater, cli : `TestManualScan_SeveralNamesKeepTheFileOrderAndUnknownIsRefused` | fait |
| test_docker_update.py | TestManualScan | `test_run_on_a_selection_updates_only_those` | 4 et 5 | internal/updater, cli : `TestManualScan_RunOnASelectionUpdatesOnlyThose` | fait |
| test_docker_update.py | TestManualScan | `test_a_selection_never_erases_what_it_did_not_look_at` | 4 et 5 | internal/updater, cli : `TestManualScan_ASelectionNeverErasesWhatItDidNotLookAt` | fait |
| test_docker_update.py | TestManualScan | `test_a_selection_keeps_the_known_errors_of_the_others` | 4 et 5 | internal/updater, cli : `TestManualScan_ASelectionKeepsTheKnownErrorsOfTheOthers` | fait |
| test_docker_update.py | TestManualScan | `test_check_notify_sends_the_state_table_of_the_selection` | 4 et 5 | internal/updater, cli : `TestManualScan_CheckNotifySendsTheStateTableOfTheSelection` | fait |
| test_docker_update.py | TestManualScan | `test_check_without_notify_still_sends_nothing` | 4 et 5 | internal/updater, cli : `TestManualScan_CheckWithoutNotifyStillSendsNothing` | fait |
| test_docker_update.py | TestManualScan | `test_check_notify_without_a_webhook_fails_loudly` | 4 et 5 | internal/updater, cli : `TestManualScan_CheckNotifyWithoutAWebhookFailsLoudly` | fait |
| test_docker_update.py | TestManualScan | `test_status_rows` | 4 et 5 | internal/updater, cli : `TestManualScan_StatusRows` | fait |
| test_e2e.py | E2E | `test_full_life` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_old_image_is_removed_after_a_successful_update_only` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_cleanup_can_be_turned_off` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_check_and_run_accept_names` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_stopped_container_is_left_alone` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_failure_alerts_without_rollback_and_exits_1` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_hold_then_status` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | E2E | `test_bad_config_and_unknown_name` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | Advisories | `test_confirmed_fix_is_applied_despite_the_long_cooldown` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | Advisories | `test_candidate_still_vulnerable_waits_the_normal_delay` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | Advisories | `test_check_does_not_pull_even_with_an_advisory` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | Advisories | `test_unreachable_api_is_not_fatal` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | ReleaseNotes | `test_alert_is_sent_once_and_nothing_is_applied` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | ReleaseNotes | `test_check_reports_but_sends_nothing` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | ReleaseNotes | `test_missing_repository_and_unreachable_api_are_not_errors` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | ReleaseNotes | `test_unreadable_version_gives_one_alert_and_no_request` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | ReleaseNotes | `test_validate_refuses_unknown_and_incomplete_release_keys` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | OneReport | `test_update_and_release_alert_arrive_in_a_single_post` | 5 | `e2e/e2e_test.go` | fait |
| test_e2e.py | SetWebhook | `test_set_webhook_through_stdin` | 5 | `e2e/e2e_test.go` | fait |

Total : 232 tests Python distincts.

Tests ajoutés par le portage (absents du Python) :

| Go | Pourquoi |
| --- | --- |
| `internal/cli` : `TestNotifyTestSendsAMessageAndNeverPrintsTheURL`, `TestValidateAndSetWebhookDoNotTakeTheLock`, `TestSetWebhookReadsOnlyTheFirstLineOfStdin`, `TestStateErrorExitsWith1`, `TestUnknownNameExitsWith1` | codes de sortie, verrou et URL du webhook : propriétés que le Python ne testait que de bout en bout |
| `internal/cli` : `TestUsageErrors`, `TestOptionPrefixesAndEqualsForm`, `TestHelpAndVersion` | parité d'`argparse` : abréviations uniques, forme `--option=valeur`, options globales avant la commande, code 2 |
| `internal/tomledit` : `TestInsertThenDeleteRestoresTheFileByteForByte` (propriété d'aller-retour sur un corpus et les `examples/`), `TestDeleteRemovesTheBlockItsGluedCommentsAndOneBlankLine`, `TestRenderedBlockIsCanonicalAndReadsBack`, `TestInsertGoesAfterTheTrailingCommentsOfTheBlock`, `TestExamplesStayValidAfterAnInsert` | phase 7 : édition textuelle non destructive ; mutation : 7 défauts, 7 détectés après ajout d'un test (insertion avant les commentaires de fin de bloc) |
| `internal/cli` : `TestList`, `TestAdd*`, `TestDelete*`, `TestEditCommandsRefuseWhileAPassIsRunning`, `TestAFileThatFailsToReadBackIsRolledBack`, `TestEditUsageErrors` | phase 7 : `list`, `add`, `delete` (aperçu, confirmation, sauvegarde, droits, retour arrière, verrou, nettoyage de l'état) ; mutation : 14 défauts, 14 détectés après ajout du test de retour arrière |
| `internal/discover` : `TestRunning*`, `TestComposeFiles*`, `TestVerify`, `TestVersionAndRepoAndSuggestions`, `TestNeverFollowByDefault` | phase 7 : relevé des conteneurs en lecture seule (jamais l'environnement), vérification du projet, table des dépôts d'avis |
| `internal/cli` : `TestInit*`, `TestAddFromARunningContainer` | phase 7 : vérifications d'installation (chaque défaut, un par un), `init` interactif par tube et non interactif, `--force`, simulation, `add --container` ; mutation : 27 défauts, 27 détectés après ajout d'un test (dossier de configuration inutilisable) |
| `tests/test_differential.py` (14 scénarios, `DU_BIN`) ; **supprimé avec le Python** (dernier état : commit `50952f1`) | exécute le script Python et le binaire Go sur le même faux Docker et le même faux serveur HTTP, compare codes, sorties, état, requêtes et corps envoyés |
| `internal/updater` : `TestExtra_HoldOfAGroupPausesEveryMember` | mutation : ignorer la pause d'un groupe (`held`) survivait à toute la suite Python |
| `internal/updater` : `TestExtra_DryRunNeverWaitsNorProbes` | mutation : en simulation, `up` attendait et sondait quand même |
| `internal/updater` : `TestExtra_AnAlertWhoseDeliveryFailedIsSentAgainThenOnlyOnce`, `TestModes_NotifyModeRetriesAfterAFailedDelivery` | proposition 3 (appliquée) ; mutation : inscrire les alertes avant l'envoi, ou ignorer l'échec, est détecté |

Mutation manuelle de la phase 4 (34 défauts introduits à la main dans `internal/updater`, suite relancée à chaque fois) : 31 détectés d'emblée,
2 survivants corrigés par les tests ci-dessus, 1 survivant équivalent : retirer `!e.FailedBefore` dans l'accélération de sécurité (`security.go`) ne
change rien, car `cycle` exclut déjà les digests en échec avant d'appliquer ; la garde est redondante (et `describe` / `statusRow` testent
`FailedBefore` en premier). La campagne complète est la phase 6 (mission §6.4).

Mutation manuelle de la phase 6 (23 défauts de plus, un à la fois, `go test ./...` relancé à chaque fois, source restaurée ensuite) :
**23 détectés d'emblée, aucun survivant.** Couvre le budget du rapport, `@` non neutralisé, mentions Discord autorisées, droits 600 du
fichier webhook, hôte du webhook et URL générique non validés, URL du webhook et jeton GitHub journalisés, message Discord non borné,
signal faible pris pour fort, dédoublonnage des alertes (fortes et faibles) supprimé, nettoyage forcé (`-f`) ou après un échec, échec non
mémorisé, sélection `check NOM` ignorée (noms et groupes), bornes des motifs, `release_notes_ignore` ignoré, pré-version et brouillon non
ignorés, et une note de release qui accélère la mise à jour. Total Go : 57 défauts introduits (phases 4 et 6) : 56 détectés (dont 2 après l'ajout
d'un test) et 1 équivalent documenté. Rejouer : voir `docs/PORTAGE.md` ci-dessus (copier le dépôt, un défaut à la fois) ; les scripts ne sont pas versionnés.

## 4 bis. Décisions du propriétaire en cours de route

- **`init` vérifie l'installation** (demande du propriétaire, phase 7) : avant toute question, `init` contrôle en lecture seule Docker,
  `compose`, `regctl` / `buildx`, binaire, dossiers et droits, fichiers du webhook et du jeton, unités systemd et minuteur (mode
  `check` ou `run`), accès au registre et à GitHub ; `init --check` rejoue ces contrôles seuls. Spécifié dans `docs/PROMPT-reecriture-go.md` §9.2
  (étape 0) et annoncé dans la section Installation du README. Implémenté en phase 7 (`internal/cli/preflight.go`, `initcmd.go`) ; `init` ne pose ni le binaire ni les unités : il dit ce qui manque.

Écarts de la phase 7 (nouvelles commandes, pas de Python à comparer) :
27. **`add` et `delete` n'acceptent pas les fichiers aux fins de ligne `\r\n`** (refus explicite) : l'édition textuelle garantit l'aller-retour octet pour octet seulement avec des `\n`.
28. **Commentaires de fin de bloc** (collés sous la dernière clé, sans ligne vide avant le bloc suivant) : `delete` ne les supprime pas (ils peuvent parler d'autre chose) ; seuls les commentaires collés *au-dessus* de l'en-tête partent avec le bloc.
29. **`--first`** : l'aller-retour `add --first` puis `delete` est exact si une ligne vide précède le premier bloc (c'est le cas des fichiers écrits par l'outil).
30. **`init` : ordre des membres d'un groupe** : les labels Docker ne donnent pas l'ordre du fichier compose (Compose range ses services par ordre alphabétique) ; `init` propose l'ordre alphabétique et demande l'ordre voulu (« Ordre de mise à jour »).
31. **`init` : vérification d'un conteneur** : en plus du nom de projet (`compose config`), `init` et `add --container` contrôlent que `compose ps -q --all SERVICE` désigne bien le conteneur : c'est exactement ce que fera l'outil.

32. **Tables et clés inconnues refusées** (proposition 2) : une table `[x]`, un `[[x]]` autre que `[[service]]` ou une clé à la racine du fichier est une erreur de configuration (code 2) ; le Python les ignorait.
33. **Alerte non envoyée = renvoyée** (proposition 3) : si l'envoi du rapport échoue, les alertes et les « prêtes » de ce rapport ne sont pas marquées signalées ; le passage suivant les renvoie. Le Python les perdait.

## 5. Propositions

Section remise au propriétaire (mission §12). Format : *quoi*, *pourquoi*, *effet sur le comportement*, *risque*, *effort*,
*recommandation*. Rien n'est appliqué sans accord.

### Proposition 1 : refuser un saut de ligne final dans les noms, dépôts et étiquettes (défaut du Python)

- **Quoi** : `NAME_RE`, `REPO_RE`, `TAG_RE`, `GHSA_RE` et l'expression des CVE se terminent par `$`, qui en Python accepte un `\n` final.
- **Pourquoi** : `re.match(NAME_RE, "web\n")` réussit (`docker-update.py:58`, `:72`, `:230`). Un nom ou un dépôt avec saut de ligne
  passerait la validation, puis casserait une clé d'état ou une ligne de journal. Montré par `TestCompactRelease` côté Go (`"v1\n2"` est
  refusé ; le Python, lui, accepterait `"v1.0\n"`).
- **Effet sur le comportement** : visible seulement pour une configuration déjà invalide en pratique (refus en plus, message identique).
- **Risque** : quasi nul. **Effort** : petit.
- **Recommandation** : faire. **État : appliquée** (déjà strict en Go dès la phase 1, écart n° 2).

### Proposition 2 : refuser les tables et clés inconnues à la racine du fichier (défaut du Python)

- **Quoi** : une table inconnue (`[setings]`) ou une clé racine n'est pas signalée ; seule `[settings]` est contrôlée.
- **Pourquoi** : `parse_config` ne lit que `data["settings"]` et `data["service"]` (`docker-update.py:372`, `:411`). Une faute de frappe dans
  l'en-tête `[settings]` fait ignorer tous les réglages (délai, webhook, fenêtre) sans un mot : l'outil tourne avec les valeurs par défaut.
  Test Go qui montre le comportement actuel : `TestStructureErrors`.
- **Effet sur le comportement** : visible : une configuration qui contient des tables ou clés inutiles est refusée (code 2) au lieu d'être acceptée.
- **Risque** : faible (un fichier existant avec une table étrangère cesserait de valider). **Effort** : petit.
- **Recommandation** : faire, après la bascule (c'est un changement de comportement).
- **État : appliquée** (`config.Load` refuse `[x]`, `[[x]]` et toute clé à la racine ; messages dans `TestStructureErrors`). Écart n° 32.

### Proposition 3 : ne pas perdre une alerte quand l'envoi échoue (défaut du Python)

- **Quoi** : une alerte (avis, notes de release) ou une version « prête » est marquée comme signalée dans l'état **avant** l'envoi du rapport. Si
  Discord est injoignable à ce moment, la ligne « notification non envoyée » est journalisée, mais l'alerte n'est plus jamais renvoyée.
- **Pourquoi** : `alert()` écrit la clé dans `state["alerts"]` et `cycle()` met `notified = True`, puis `flush()` envoie (`docker-update.py:1160-1172`,
  `:1555-1561`, `:1153`). Une panne d'une heure pendant qu'un avis critique est détecté le fait taire pour de bon. Montré par
  `TestExtra_AnAlertWhoseDeliveryFailedIsNotSentAgain` côté Go.
- **Effet sur le comportement** : visible seulement après un échec d'envoi : le rapport non remis serait renvoyé au passage suivant (une seule fois par alerte réussie).
- **Risque** : faible (il faut que `Send` dise s'il a réussi, et ne marquer les clés qu'après). **Effort** : moyen (`Notifier.Send` renvoie un booléen, `flush` valide les clés).
- **Recommandation** : faire, après la bascule (c'est un changement de comportement ; les mises à jour appliquées ne sont pas concernées, seulement l'information).
- **État : appliquée** : `Notifier.Send` renvoie un booléen ; `alert()` et la liste des versions « prêtes » ne font que préparer des clés ; `flush()` les inscrit dans l'état seulement si l'envoi a réussi. Écart n° 33.

### Proposition 4 : publier des versions (binaires et sommes de contrôle) depuis la CI
- **Quoi** : un workflow déclenché par une étiquette `v*` qui compile amd64 et arm64 et joint les binaires et `SHA256SUMS` à une release GitHub.
- **Pourquoi** : aujourd'hui aucune version n'existe ; on compile ou on prend l'artefact de la CI (expire). `docker-update version` affiche `dev` hors CI.
- **Effet sur le comportement** : aucun sur l'outil. **Risque** : faible (permissions `contents: write` pour ce seul workflow). **Effort** : petit.
- **Recommandation** : faire. **État : appliquée** (`.github/workflows/release.yml` : étiquette `v*` → tests, binaires amd64 et arm64, `SHA256SUMS`, release ; version injectée par `-X main.version`). Non exécutable avant la première étiquette : à vérifier avec `v0.1.0` après fusion.

### Proposition 5 : `golangci-lint` en CI
- **Quoi** : ajouter un lint en plus de `gofmt` et `go vet`. **Pourquoi** : attrape des erreurs ignorées et du code mort que `vet` laisse passer.
- **Effet** : aucun sur l'outil. **Risque** : outil de développement tiers dans la CI (pas dans le module). **Effort** : petit.
- **Recommandation** : à discuter (accord demandé par la mission §13).

### Proposition 6 : lecture directe du registre, sans `buildx` ni `regctl`
- **Quoi** : interroger l'API du registre en Go (requête `HEAD` sur le manifeste) et lire plusieurs images en parallèle.
- **Pourquoi** : supprime une dépendance d'exécution et accélère les passages de nombreux services.
- **Effet** : visible seulement en vitesse. **Risque** : moyen à élevé : il faudrait refaire l'authentification (jeton de `DOCKER_CONFIG`, limites Docker Hub), surface de sécurité nouvelle.
- **Effort** : gros. **Recommandation** : ne pas faire pour l'instant (la parité avec `buildx` / `regctl` est éprouvée).

### Proposition 7 : commandes de configuration supplémentaires (phase 7)
- **Quoi** : `rename` (renommer un service ou un groupe en gardant état et pauses), `edit` (ouvrir le fichier avec validation avant écriture), export de la configuration, et vérification qu'un dépôt GitHub de `advisories` / `release_notes_repos` existe (`init --check` ou `validate --online`).
- **Pourquoi** : renommer à la main laisse des entrées orphelines dans l'état ; une faute de frappe sur un dépôt n'est vue qu'au passage suivant (réponse 404 traitée comme vide).
- **Effet** : nouvelles commandes seulement. **Risque** : faible. **Effort** : moyen.
- **Recommandation** : `rename` et la vérification des dépôts : faire ; `edit` et l'export : à discuter.

