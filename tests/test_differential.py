"""Test différentiel : le script Python et le binaire Go (DU_BIN) rejouent les mêmes scénarios, chacun dans son propre monde.

Après CHAQUE commande on compare : code de sortie, sortie standard (durées lissées), journal des commandes docker reçues par le
faux `docker` (arguments exacts), état du faux monde, fichier d'état de l'outil, requêtes reçues par le faux GitHub (chemin et
en-têtes utiles) et messages reçus par le faux webhook.

    DU_BIN=/chemin/docker-update python3 -m unittest tests.test_differential -v
"""
import datetime, hashlib, http.server, json, os, pathlib, re, shutil, subprocess, sys, tempfile, threading, unittest

HERE = pathlib.Path(__file__).resolve().parent
SCRIPT = HERE.parent / "docker-update.py"
SHIM = HERE / "e2e"
DU_BIN = os.environ.get("DU_BIN")


def man(n):
    return '{"schemaVersion":2,"n":%d}\n' % n


def dig(n):
    return "sha256:" + hashlib.sha256(man(n).encode()).hexdigest()


class Server:
    """Faux GitHub + faux webhook sur 127.0.0.1. Les requêtes sont enregistrées ; ETag et 304 sont gérés."""

    KEEP = ("authorization", "accept", "user-agent", "x-github-api-version", "if-none-match", "content-type", "title", "priority")

    def __init__(self):
        self.routes = {}
        self.events = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def _record(self, body=None):
                headers = {k.lower(): v for k, v in self.headers.items() if k.lower() in outer.KEEP}
                outer.events.append({"method": self.command, "path": self.path, "headers": headers, "body": body})

            def _reply(self, status, data=b"", etag=""):
                self.send_response(status)
                if etag:
                    self.send_header("ETag", etag)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            def do_GET(self):
                self._record()
                for prefix, (code, body, etag) in outer.routes.items():
                    if self.path.startswith(prefix):
                        if code == 200 and etag and self.headers.get("If-None-Match") == etag:
                            return self._reply(304, b"", etag)
                        return self._reply(code, body if code == 200 else b"{}", etag)
                self._reply(404, b"{}")

            def do_POST(self):
                raw = self.rfile.read(int(self.headers["Content-Length"]))
                try:
                    body = json.loads(raw)
                except ValueError:
                    body = raw.decode()
                self._record(body)
                self._reply(204)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.api = f"http://127.0.0.1:{self.srv.server_address[1]}"

    def route(self, prefix, items, code=200, etag=""):
        self.routes[prefix] = (code, json.dumps(items).encode(), etag)

    def take(self):
        ev, self.events = self.events, []
        return ev

    def close(self):
        self.srv.shutdown()
        self.srv.server_close()


class Rig:
    def __init__(self, program, name):
        self.program = program
        self.tmp = pathlib.Path(tempfile.mkdtemp(prefix=f"du-diff-{name}-"))
        self.world_file = self.tmp / "world.json"
        self.state = self.tmp / "state.json"
        self.cfg = self.tmp / "updates.toml"
        self.lock = self.tmp / "lock"

    def sub(self, text, api=""):
        return (text.replace("@TMP@", str(self.tmp)).replace("@STATE@", str(self.state)).replace("@LOCK@", str(self.lock))
                .replace("@API@", api))

    def write_world(self, world, api=""):
        self.world_file.write_text(self.sub(json.dumps(world), api))

    def world(self):
        return json.loads(self.world_file.read_text())

    def run(self, args, stdin=None):
        env = dict(os.environ, PATH=f"{SHIM}:{os.environ['PATH']}", SHIM_STATE=str(self.world_file))
        return subprocess.run([*self.program, "--config", str(self.cfg), "--allow-untrusted-config", *args], input=stdin,
                              capture_output=True, text=True, env=env, timeout=120)


AGE = re.compile(r"\b(\d+) (s|min)\b")


def smooth(text, rig):
    """Chemins du banc et petites durées (le temps réel passe entre deux commandes) rendus comparables."""
    text = text.replace(str(rig.tmp), "<TMP>")
    text = re.sub(r"(depuis|encore) \d+ (s|min|h|j)", r"\1 ~", text)
    return AGE.sub(r"N \2", text)


def norm_state(path, rig):
    if not path.exists():
        return None
    d = json.loads(smooth(path.read_text(), rig))
    for img in d["images"].values():
        img["first_seen"] = 0
    d["alerts"] = {k: 0 for k in d["alerts"]}
    for bucket in ("advisories", "releases"):
        for rec in d[bucket].values():
            rec["fetched"] = 0
            for k in ("etag", "err", "full", "ok"):       # Go omet les clés vides (écart n° 12)
                if not rec.get(k):
                    rec.pop(k, None)
            if "full" in rec:
                rec["full"] = 0
    return d


def norm_events(events):
    out = []
    for e in events:
        e = dict(e)
        if isinstance(e["body"], dict):
            e["body"] = json.loads(json.dumps(e["body"]))
        else:
            e["body"] = AGE.sub(r"N \2", e["body"]) if isinstance(e["body"], str) else e["body"]
            # urllib ajoute un Content-Type de formulaire à tout POST : sans effet pour ntfy (écart documenté)
            e["headers"] = {k: v for k, v in e["headers"].items() if k != "content-type"}
        out.append(e)
    return out


@unittest.skipUnless(DU_BIN, "DU_BIN non défini : pas de binaire Go à comparer")
class Differential(unittest.TestCase):
    def setUp(self):
        self.py = Rig([sys.executable, "-I", str(SCRIPT)], "py")
        self.go = Rig([DU_BIN], "go")
        self.rigs = (self.py, self.go)
        self.server = Server()
        self.addCleanup(self.server.close)
        for r in self.rigs:
            self.addCleanup(shutil.rmtree, r.tmp, True)
        self.step_no = 0

    # -- monde et configuration ------------------------------------------------

    def world(self, remote=2, fail_up=(), old_version="", new_version="", status="running", extra=None):
        w = {"registry": {"traefik:v3.6": man(remote)}, "images": {"id-old": ["traefik@" + dig(1)]},
             "tags": {"traefik:v3.6": "id-old"}, "by": {},
             "containers": {"@TMP@/traefik|traefik": {"id": "cid1", "image_id": "id-old", "status": status}},
             "compose": {"@TMP@/traefik": {"traefik": "traefik:v3.6"}}, "fail_up": list(fail_up),
             "labels": {"id-old": {"org.opencontainers.image.version": old_version}} if old_version else {},
             "digest_versions": {dig(remote): new_version} if new_version else {}}
        w.update(extra or {})
        for r in self.rigs:
            (r.tmp / "traefik").mkdir(exist_ok=True)
            r.write_world(w, self.server.api)

    def config(self, services, settings=""):
        text = f'[settings]\nstate_file = "@STATE@"\nlock_file = "@LOCK@"\nsettle_seconds = 0\ngithub_api = "@API@"\n{settings}\n{services}\n'
        for r in self.rigs:
            r.cfg.write_text(r.sub(text, self.server.api))

    TRAEFIK = '[[service]]\nname = "traefik"\ncompose_dir = "@TMP@/traefik"\nservice = "traefik"\nmode = "auto"\n{}\n'

    def svc(self, extra="", cooldown="0"):
        return self.TRAEFIK.format(f'cooldown = "{cooldown}"\n{extra}')

    def hook(self, fmt="discord"):
        """Déclare un webhook (le faux serveur) dans les deux bancs."""
        for r in self.rigs:
            (r.tmp / "hook").write_text(self.server.api + "/hook/SECRET-TOKEN\n")
        return f'notify_url_file = "@TMP@/hook"\nnotify_format = "{fmt}"'

    # -- comparaison -----------------------------------------------------------

    def both(self, *args, stdin=None, same_stderr=True):
        self.step_no += 1
        label = f"étape {self.step_no} : {' '.join(args)}"
        res = []
        for r in self.rigs:
            self.server.take()
            p = r.run(list(args), stdin)
            res.append((r, p, self.server.take()))
        (rp, pp, ep), (rg, pg, eg) = res
        self.assertEqual(pg.returncode, pp.returncode, f"{label}\npython : {pp.stdout}{pp.stderr}\ngo : {pg.stdout}{pg.stderr}")
        if pp.returncode != 2:
            self.assertEqual(smooth(pg.stdout, rg), smooth(pp.stdout, rp), f"{label} : sortie standard")
            if same_stderr:
                self.assertEqual((pp.stderr, pg.stderr), ("", ""), f"{label} : sortie d'erreur")
        for secret in ("SECRET-TOKEN", "hunter2", "jeton-SUPER-SECRET"):
            self.assertNotIn(secret, pg.stdout + pg.stderr, f"{label} : secret dans la sortie du Go")
        self.assertEqual(norm_events(eg), norm_events(ep), f"{label} : requêtes HTTP")
        wp, wg = (json.loads(smooth(r.world_file.read_text(), r)) if r.world_file.exists() else None for r in (rp, rg))
        self.assertEqual(wg, wp, f"{label} : monde docker (commandes reçues et état)")
        self.assertEqual(norm_state(rg.state, rg), norm_state(rp.state, rp), f"{label} : fichier d'état")
        return pg

    # -- scénarios -------------------------------------------------------------

    def test_full_life(self):
        self.world(); self.config(self.svc())
        self.both("validate")
        self.both("--dry-run", "run")
        self.config(self.svc(cooldown="1d"))
        self.both("check")
        self.both("run")
        self.both("now", "traefik")
        self.both("now", "traefik", "--yes")
        self.both("run")
        self.both("status")

    def test_cleanup_failure_and_retry(self):
        self.world(); self.config(self.svc())
        self.both("run")
        self.world(remote=3, fail_up=["id-" + dig(3)[7:19]])
        self.both("run")
        self.both("run")
        self.both("now", "traefik", "--yes")
        self.world(); self.config(self.svc("cleanup = false"))
        self.both("run")

    def test_stopped_container(self):
        self.world(status="exited"); self.config(self.svc())
        for args in (("run",), ("check",), ("now", "traefik", "--yes"), ("check", "traefik")):
            self.both(*args)
        self.world(status="restarting")
        self.both("run")

    def test_hold_and_names(self):
        self.world(); self.config(self.svc(cooldown="1d"))
        self.both("hold", "traefik")
        self.both("run")
        self.both("status")
        self.both("now", "traefik", "--yes")
        self.both("unhold", "traefik")
        self.both("check", "traefik")
        self.both("check", "inconnu")
        self.both("now", "inconnu", "--yes")
        self.both("hold", "inconnu")
        self.both("status")

    def test_configuration_errors(self):
        self.world()
        self.config(self.svc("oups = 1"))
        self.both("validate")
        self.config(self.svc("release_notes = true"))
        self.both("validate")
        self.config(self.svc('mode = "turbo"'))
        self.both("run")
        self.config(self.svc("pre_hook = \"x\""))
        self.both("validate")
        self.config(self.svc(), settings='heartbeat_url_file = "x"')
        self.both("validate")

    def test_advisory_fast_track_with_etag_and_token(self):
        published = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=7)).isoformat()
        adv = [{"ghsa_id": "GHSA-2222-3333-4444", "cve_id": "CVE-2026-12345", "severity": "high", "published_at": published,
                "summary": "Contournement d'authentification", "vulnerabilities": [{"vulnerable_version_range": "< 3.6.3"}]}]
        self.server.route("/repos/acme/proxy/security-advisories", adv, etag='"adv-1"')
        for r in self.rigs:
            (r.tmp / "token").write_text("jeton-SUPER-SECRET\n")
        self.world(old_version="3.6.2", new_version="3.6.3")
        self.config(self.svc('advisories = ["acme/proxy"]\nsecurity_cooldown = "1h"\ncooldown = "3d"'),
                    settings='github_token_file = "@TMP@/token"\nadvisory_poll = "0"\n' + self.hook())
        self.both("check")
        self.both("run")
        self.both("run")                                      # revalidation conditionnelle (304)
        self.world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.both("run")

    def test_advisory_candidate_still_vulnerable_and_unreadable(self):
        published = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=7)).isoformat()
        adv = [{"ghsa_id": "GHSA-2222-3333-4444", "severity": "critical", "published_at": published, "summary": "x",
                "vulnerabilities": [{"vulnerable_version_range": "< 3.6.3"}, {"vulnerable_version_range": "bizarre"}]}]
        self.server.route("/repos/acme/proxy/security-advisories", adv)
        for nv in ("3.6.2", ""):
            self.world(old_version="3.6.2", new_version=nv)
            self.config(self.svc('advisories = ["acme/proxy"]\ncooldown = "3d"'), settings=self.hook())
            self.both("run")
            self.both("run")
        self.world(old_version="")
        self.both("run")

    def test_release_notes_report_and_dedup(self):
        when = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=3)).isoformat()

        def rel(tag, body, **kw):
            return {"tag_name": tag, "body": body, "published_at": when, "prerelease": False, "draft": False, **kw}
        self.server.route("/repos/acme/proxy/releases", [
            rel("v3.6.4", "Fixes CVE-2026-12345 and a path traversal. @everyone " + "x " * 200),
            rel("v3.6.3", "better security"), rel("v3.6.1", "CVE-2020-0001 older"), rel("v3.7.0-rc1", "CVE-2026-9", prerelease=True),
            rel("nightly", "CVE-2026-8"), rel("v3.6.5", "draft", draft=True, published_at=None)], etag='"rel-1"')
        self.world(remote=2, old_version="3.6.2", new_version="3.6.3")
        self.config(self.svc('release_notes = true\nrelease_notes_repos = ["acme/proxy"]\nrelease_notes_ignore = ["nothing here"]'),
                    settings='advisory_poll = "0"\n' + self.hook())
        self.both("check")
        self.both("run")
        self.both("run")
        self.world(remote=2, old_version="3.6.2")
        self.both("run")

    def test_release_api_failures(self):
        self.world(old_version="3.6.2"); self.hook()
        for code in (404, 500, 403, 429):
            self.server.route("/repos/acme/proxy/releases", [], code=code)
            self.config(self.svc('release_notes = true\nrelease_notes_repos = ["acme/proxy"]'), settings=self.hook())
            self.both("run")
            self.world(old_version="3.6.2")
            self.both("run")
            for r in self.rigs:
                r.state.unlink(missing_ok=True)

    def test_unreadable_version_and_check_notify(self):
        self.server.route("/repos/acme/proxy/releases", [{"tag_name": "v3.6.3", "body": "CVE-2026-1", "prerelease": False,
                                                          "draft": False, "published_at": None}])
        self.world(); self.config(self.svc('release_notes = true\nrelease_notes_repos = ["acme/proxy"]', cooldown="1d"),
                                  settings=self.hook())
        self.both("run")
        self.both("run")
        self.both("check", "--notify")
        self.both("check", "traefik", "--notify")

    def test_ntfy_and_json_formats(self):
        self.world(); self.config(self.svc(), settings=self.hook("ntfy"))
        self.both("run")
        self.world(); self.config(self.svc(), settings=self.hook("json"))
        for r in self.rigs:
            r.state.unlink(missing_ok=True)
        self.both("run")
        self.both("notify-test")

    def test_group_window_and_ready_notification(self):
        def stack(names, **kw):
            world = {"registry": {"ghcr.io/org/app:main": man(2)}, "images": {}, "tags": {"ghcr.io/org/app:main": "id-old"}, "by": {},
                     "containers": {}, "compose": {"@TMP@/app": {}}, "fail_up": kw.get("fail_up", []), "labels": {}, "digest_versions": {}}
            for n in names:
                world["images"]["id-old"] = ["ghcr.io/org/app@" + dig(1)]
                world["containers"][f"@TMP@/app|{n}"] = {"id": "cid-" + n, "image_id": "id-old", "status": "running"}
                world["compose"]["@TMP@/app"][n] = "ghcr.io/org/app:main"
            for r in self.rigs:
                (r.tmp / "app").mkdir(exist_ok=True)
                r.write_world(world)
        names = ("migrate", "web", "admin")
        svcs = "".join(f'[[service]]\nname = "{n}"\ngroup = "app"\ncompose_dir = "@TMP@/app"\nservice = "{n}"\nmode = "auto"\ncooldown = "0"\n'
                       f'window = "00:00-23:59"\nsmoke_url = "{self.server.api}/smoke"\nsmoke_expect = [404]\n' for n in names)
        stack(names); self.config(svcs, settings=self.hook())
        self.both("run")
        stack(names, fail_up=["id-" + dig(2)[7:19]])
        for r in self.rigs:
            r.state.unlink(missing_ok=True)
        self.both("run")
        self.both("run", "app")
        self.both("check", "web", "admin", "--notify")
        notify_only = svcs.replace('mode = "auto"', 'mode = "notify"').replace('window = "00:00-23:59"', 'window = ""')
        stack(names); self.config(notify_only, settings=self.hook())
        for r in self.rigs:
            r.state.unlink(missing_ok=True)
        self.both("run")
        self.both("run")

    def test_set_webhook(self):
        url = "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
        self.world()
        self.config(self.svc(), settings='notify_url_file = "@TMP@/conf/discord_webhook"')
        for stdin in (url + "\nautre\n", "pas une url\n", "", url):
            pg = self.both("set-webhook", stdin=stdin)
            self.assertNotIn("AbCdEfGh", pg.stdout + pg.stderr)
        for r in self.rigs:
            f = r.tmp / "conf" / "discord_webhook"
            self.assertEqual((f.read_text(), f.stat().st_mode & 0o777), (url + "\n", 0o600))
        self.both("--dry-run", "set-webhook", stdin=url)
        self.config(self.svc())
        self.both("set-webhook", stdin=url)

    def test_lock_and_no_command_issued_by_validate_status_hold(self):
        self.world(); self.config(self.svc())
        for args in (("validate",), ("status",), ("hold", "traefik"), ("unhold", "traefik")):
            self.both(*args)
            for r in self.rigs:
                self.assertEqual(r.world().get("log", []) if args[0] != "validate" else [], [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
