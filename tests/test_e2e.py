"""Test de bout en bout : le vrai programme, en sous-processus, avec un faux `docker` dans le PATH."""
import datetime, hashlib, http.server, json, os, pathlib, subprocess, sys, tempfile, threading, unittest

HERE = pathlib.Path(__file__).resolve().parent
SCRIPT = HERE.parent / "docker-update.py"
SHIM = HERE / "e2e"


def man(n):
    return '{"schemaVersion":2,"n":%d}\n' % n


def dig(n):
    return "sha256:" + hashlib.sha256(man(n).encode()).hexdigest()


class E2E(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        t = pathlib.Path(self.tmp.name)
        self.dir = t / "traefik"
        self.dir.mkdir()
        self.shim_state = t / "world.json"
        self.state = t / "state.json"
        self.cfg = t / "updates.toml"
        self.write_world(remote=2)
        self.write_cfg("0")

    def write_cfg(self, cooldown, extra=""):
        self.cfg.write_text(f'''[settings]
state_file = "{self.state}"
lock_file = "{self.tmp.name}/lock"
settle_seconds = 0
[[service]]
name = "traefik"
compose_dir = "{self.dir}"
service = "traefik"
mode = "auto"
cooldown = "{cooldown}"
{extra}
''')

    def write_world(self, remote, fail_up=(), old_version="", new_version=""):
        with open(self.shim_state, "w") as f:
            json.dump({
                "registry": {"traefik:v3.6": man(remote)},
                "images": {"id-old": ["traefik@" + dig(1)]},
                "tags": {"traefik:v3.6": "id-old"}, "by": {},
                "containers": {f"{self.dir}|traefik": {"id": "cid1", "image_id": "id-old", "status": "running"}},
                "compose": {str(self.dir): {"traefik": "traefik:v3.6"}},
                "fail_up": list(fail_up),
                "labels": {"id-old": {"org.opencontainers.image.version": old_version}} if old_version else {},
                "digest_versions": {dig(remote): new_version} if new_version else {},
            }, f)

    def world(self):
        with open(self.shim_state) as f:
            return json.load(f)

    def du(self, *args):
        env = dict(os.environ, PATH=f"{SHIM}:{os.environ['PATH']}", SHIM_STATE=str(self.shim_state))
        return subprocess.run([sys.executable, "-I", str(SCRIPT), "--config", str(self.cfg),
                               "--allow-untrusted-config", *args],
                              capture_output=True, text=True, env=env, timeout=60)

    def image(self):
        return self.world()["containers"][f"{self.dir}|traefik"]["image_id"]

    def test_full_life(self):
        r = self.du("validate")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("OK", r.stdout)

        r = self.du("--dry-run", "run")                       # simulation : rien ne bouge
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("[simulation]", r.stdout)
        self.assertEqual(self.image(), "id-old")
        self.assertFalse(self.state.exists())

        self.write_cfg("1d")
        r = self.du("check")                                  # observe seulement
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("différée", r.stdout)
        self.assertEqual(self.image(), "id-old")
        with open(self.state) as f:
            self.assertIn("traefik:v3.6", json.load(f)["images"])

        r = self.du("run")                                    # délai pas écoulé : rien
        self.assertEqual(self.image(), "id-old")

        r = self.du("now", "traefik")                         # sans --yes : simulation
        self.assertEqual(self.image(), "id-old")
        self.assertIn("--yes", r.stdout)

        r = self.du("now", "traefik", "--yes")                # voie rapide
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.image(), "id-" + dig(2)[7:19])
        pulls = [c for c in self.world()["log"] if c[:1] == ["pull"]]
        self.assertEqual(pulls, [["pull", "traefik@" + dig(2)]])

        r = self.du("run")                                    # tout est à jour
        self.assertEqual(r.returncode, 0)
        self.assertIn("à jour", r.stdout)
        self.assertNotIn("hunter2", r.stdout + r.stderr, "le contenu de compose config ne doit jamais sortir")

    def test_old_image_is_removed_after_a_successful_update_only(self):
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.world()["removed"], ["id-old"])
        self.assertIn("nettoyage : image précédente", r.stdout)
        self.write_world(remote=3, fail_up=["id-" + dig(3)[7:19]])
        r = self.du("run")
        self.assertEqual(r.returncode, 1)
        self.assertNotIn("removed", self.world(), "échec : rien n'est supprimé")

    def test_cleanup_can_be_turned_off(self):
        self.write_cfg("0", "cleanup = false")
        self.assertEqual(self.du("run").returncode, 0)
        self.assertEqual(self.image(), "id-" + dig(2)[7:19])
        self.assertNotIn("removed", self.world())

    def test_stopped_container_is_left_alone(self):
        w = self.world()
        w["containers"][f"{self.dir}|traefik"]["status"] = "exited"
        with open(self.shim_state, "w") as f:
            json.dump(w, f)
        for args in (("run",), ("check",), ("now", "traefik", "--yes")):
            r = self.du(*args)
            self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        r = self.du("run")
        self.assertIn("conteneur arrêté (exited), non touché", r.stdout)
        self.assertEqual(self.image(), "id-old")
        self.assertEqual(self.world()["containers"][f"{self.dir}|traefik"]["status"], "exited")
        log = self.world()["log"]
        self.assertEqual([c for c in log if c[:1] in (["pull"], ["tag"]) or c[:2] == ["image", "rm"] or "up" in c], [])
        self.assertEqual([c for c in log if c[:1] == ["buildx"]], [], "le registre n'est même pas interrogé")

    def test_failure_alerts_without_rollback_and_exits_1(self):
        self.write_world(remote=2, fail_up=["id-" + dig(2)[7:19]])
        r = self.du("run")
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertIn("Aucun retour arrière automatique", r.stdout)
        ups = [c for c in self.world()["log"] if c[:1] == ["compose"] and "up" in c]
        self.assertEqual(len(ups), 1, "un seul `up` : pas de second pour revenir en arrière")
        self.assertEqual(self.world()["tags"]["traefik:v3.6"], "id-" + dig(2)[7:19], "l'étiquette n'est pas remise en arrière")
        r = self.du("run")                                    # pas de nouvelle tentative sur le même digest
        self.assertIn("déjà tentée", r.stdout)
        self.assertEqual(r.returncode, 0)

    def test_hold_then_status(self):
        self.assertEqual(self.du("hold", "traefik").returncode, 0)
        r = self.du("run")
        self.assertIn("en pause", r.stdout)
        self.assertEqual(self.image(), "id-old")
        self.assertIn("traefik", self.du("status").stdout)
        self.assertEqual(self.du("unhold", "traefik").returncode, 0)
        self.du("run")
        self.assertEqual(self.image(), "id-" + dig(2)[7:19])

    def test_bad_config_and_unknown_name(self):
        self.write_cfg("0", 'oups = 1')
        self.assertEqual(self.du("validate").returncode, 2)
        self.write_cfg("0")
        self.assertEqual(self.du("now", "inconnu", "--yes").returncode, 1)


class Advisories(E2E):
    """Avis de sécurité servis par un vrai serveur HTTP local ; le programme lit via le vrai urllib."""

    def serve(self, rng="< 3.6.3", sev="high"):
        published = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=3)).isoformat()
        body = json.dumps([{"ghsa_id": "GHSA-2222-3333-4444", "cve_id": "CVE-2026-12345", "severity": sev,
                            "published_at": published, "summary": "Contournement d'authentification",
                            "vulnerabilities": [{"vulnerable_version_range": rng}]}]).encode()
        self.hits = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                outer.hits.append(self.path)
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        return f"http://127.0.0.1:{srv.server_address[1]}"

    def cfg_with(self, api, cooldown="3d", extra=""):
        self.cfg.write_text(f'''[settings]
state_file = "{self.state}"
lock_file = "{self.tmp.name}/lock"
settle_seconds = 0
github_api = "{api}"
[[service]]
name = "traefik"
compose_dir = "{self.dir}"
service = "traefik"
mode = "auto"
cooldown = "{cooldown}"
advisories = ["acme/proxy"]
security_cooldown = "0"
{extra}
''')

    def test_confirmed_fix_is_applied_despite_the_long_cooldown(self):
        api = self.serve()
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.cfg_with(api)
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("CORRECTIF DE SÉCURITÉ", r.stdout)
        self.assertEqual(self.image(), "id-" + dig(2)[7:19], "appliqué alors que le délai normal est de 3 jours")
        self.assertEqual(self.hits, ["/repos/acme/proxy/security-advisories?per_page=50"])

    def test_candidate_still_vulnerable_waits_the_normal_delay(self):
        api = self.serve()
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.2")
        self.cfg_with(api)
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.image(), "id-old")
        self.assertIn("pas de correctif dans la nouvelle image", r.stdout)

    def test_check_does_not_pull_even_with_an_advisory(self):
        api = self.serve()
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.cfg_with(api)
        r = self.du("check")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual([c for c in self.world()["log"] if c[:1] == ["pull"]], [])
        self.assertIn("prochain passage", r.stdout)

    def test_unreachable_api_is_not_fatal(self):
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.cfg_with("http://127.0.0.1:9")
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("injoignable", r.stdout)
        self.assertEqual(self.image(), "id-old", "sans avis, le délai normal s'applique")


class ReleaseNotes(E2E):
    """Notes de release servies par un vrai serveur HTTP local ; le webhook est un second endpoint du même serveur."""

    def serve(self, releases, code=200):
        self.gets, self.posts = [], []
        body = json.dumps(releases).encode()
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                outer.gets.append(self.path)
                if "/releases" not in self.path:
                    return self._reply(404, b"{}")
                self._reply(code, body if code == 200 else b"{}")

            def do_POST(self):
                outer.posts.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
                self._reply(204, b"")

            def _reply(self, status, data):
                self.send_response(status)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=srv.serve_forever, daemon=True).start()
        self.addCleanup(srv.server_close)
        self.addCleanup(srv.shutdown)
        return f"http://127.0.0.1:{srv.server_address[1]}"

    def cfg_with(self, api, cooldown="3d", extra=""):
        hook = pathlib.Path(self.tmp.name) / "webhook"
        hook.write_text(api + "/hook/SECRET-TOKEN\n")
        self.cfg.write_text(f'''[settings]
state_file = "{self.state}"
lock_file = "{self.tmp.name}/lock"
settle_seconds = 0
github_api = "{api}"
notify_url_file = "{hook}"
[[service]]
name = "traefik"
compose_dir = "{self.dir}"
service = "traefik"
mode = "auto"
cooldown = "{cooldown}"
release_notes = true
release_notes_repos = ["acme/proxy"]
{extra}
''')

    @staticmethod
    def rel(tag, body, **kw):
        when = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=3)).isoformat()
        return {"tag_name": tag, "body": body, "published_at": when, "prerelease": False, "draft": False, **kw}

    def test_alert_is_sent_once_and_nothing_is_applied(self):
        api = self.serve([self.rel("v3.6.3", "Fixes CVE-2026-12345 and a path traversal"),
                          self.rel("v3.6.1", "CVE-2020-0001 (older than the running version)"),
                          self.rel("v3.7.0-rc1", "CVE-2026-9999", prerelease=True)])
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.cfg_with(api)
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(self.gets, ["/repos/acme/proxy/releases?per_page=30"])
        self.assertEqual(len(self.posts), 1, r.stdout)
        content = self.posts[0]["content"]
        for want in ("3.6.2 → v3.6.3", "CVE-2026-12345", "path traversal", "https://github.com/acme/proxy/releases/tag/v3.6.3"):
            self.assertIn(want, content)
        self.assertNotIn("CVE-2020-0001", content)
        self.assertEqual(self.posts[0]["allowed_mentions"], {"parse": []})
        self.assertEqual(self.image(), "id-old", "une note de release n'accélère rien : délai de 3 jours en cours")
        self.assertEqual([c for c in self.world()["log"] if c[:1] == ["pull"]], [])
        self.assertNotIn("SECRET-TOKEN", r.stdout + r.stderr)
        r = self.du("run")                                    # nouveau processus : l'état disque dédoublonne
        self.assertEqual(len(self.posts), 1)
        with open(self.state) as f:
            st = json.load(f)
        self.assertIn("release:traefik:v3.6.3", st["alerts"])
        self.assertEqual(len(st["releases"]["acme/proxy"]["items"]), 3)

    def test_check_reports_but_sends_nothing(self):
        api = self.serve([self.rel("v3.6.3", "Fixes CVE-2026-12345")])
        self.write_world(remote=2, old_version="3.6.2")
        self.cfg_with(api)
        r = self.du("check")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertIn("notes de release traefik: version détectée 3.6.2", r.stdout)
        self.assertIn("ALERTE notes de release", r.stdout)
        self.assertEqual(self.posts, [])

    def test_missing_repository_and_unreachable_api_are_not_errors(self):
        for code, api in ((404, None), (500, None), (200, "http://127.0.0.1:9")):
            with self.subTest(code=code):
                api = api or self.serve([], code=code)
                self.write_world(remote=2, old_version="3.6.2")
                self.cfg_with(api, cooldown="0")
                r = self.du("run")
                self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
                self.assertEqual(self.image(), "id-" + dig(2)[7:19], "la mise à jour normale n'est jamais empêchée")
                self.assertNotIn("ERREUR", r.stdout)
                self.state.unlink(missing_ok=True)

    def test_unreadable_version_gives_one_alert_and_no_request(self):
        api = self.serve([self.rel("v3.6.3", "Fixes CVE-2026-12345")])
        self.write_world(remote=2)                            # image sans étiquette de version
        self.cfg_with(api)
        self.du("run")
        self.du("run")
        self.assertEqual(len(self.posts), 1)
        self.assertIn("Impossible de comparer", self.posts[0]["content"])
        self.assertEqual(self.gets, [])

    def test_validate_refuses_unknown_and_incomplete_release_keys(self):
        self.write_cfg("0", "release_notes = true")           # aucun dépôt à lire
        r = self.du("validate")
        self.assertEqual(r.returncode, 2, r.stdout)
        self.assertIn("release_notes", r.stdout)
        self.write_cfg("0", "release_note = true")            # faute de frappe
        self.assertEqual(self.du("validate").returncode, 2)


class OneReport(ReleaseNotes):
    def test_update_and_release_alert_arrive_in_a_single_post(self):
        api = self.serve([self.rel("v3.6.3", "Fixes CVE-2026-12345")])
        self.write_world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.cfg_with(api, cooldown="0")
        r = self.du("run")
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(len(self.posts), 1, "UN seul message pour le passage")
        content = self.posts[0]["content"]
        self.assertIn("1 mise à jour", content)
        self.assertIn("Mises à jour réussies", content)
        self.assertIn("Notes de release à lire", content)
        self.assertIn("CVE-2026-12345", content)
        self.assertNotIn("SECRET-TOKEN", r.stdout + r.stderr)
        self.assertEqual(self.du("run").returncode, 0)
        self.assertEqual(len(self.posts), 1, "rien de nouveau : aucun message")


class SetWebhook(E2E):
    URL = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"

    def test_set_webhook_through_stdin(self):
        target = pathlib.Path(self.tmp.name) / "conf" / "discord_webhook"
        self.write_cfg("0")
        self.cfg.write_text(self.cfg.read_text().replace("settle_seconds = 0", f'settle_seconds = 0\nnotify_url_file = "{target}"'))
        env = dict(os.environ, PATH=f"{SHIM}:{os.environ['PATH']}", SHIM_STATE=str(self.shim_state))
        r = subprocess.run([sys.executable, "-I", str(SCRIPT), "--config", str(self.cfg), "--allow-untrusted-config", "set-webhook"],
                           input=self.URL + "\n", capture_output=True, text=True, env=env, timeout=60)
        self.assertEqual(r.returncode, 0, r.stdout + r.stderr)
        self.assertEqual(target.read_text(), self.URL + "\n")
        self.assertEqual(target.stat().st_mode & 0o777, 0o600)
        self.assertNotIn("AbCdEfGh", r.stdout + r.stderr)
        self.assertEqual(self.world().get("log", []), [], "aucune commande docker lancée")


if __name__ == "__main__":
    unittest.main(verbosity=2)
