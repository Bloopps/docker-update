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

    def test_failure_rolls_back_and_exits_1(self):
        self.write_world(remote=2, fail_up=["id-" + dig(2)[7:19]])
        r = self.du("run")
        self.assertEqual(r.returncode, 1, r.stdout + r.stderr)
        self.assertEqual(self.image(), "id-old")
        self.assertEqual(self.world()["tags"]["traefik:v3.6"], "id-old")
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


if __name__ == "__main__":
    unittest.main(verbosity=2)
