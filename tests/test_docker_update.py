"""Tests de docker-update avec un faux monde Docker (registre, images, conteneurs, compose).

    python3 -m unittest discover -s tests -v      (depuis la racine du dépôt)
"""
from __future__ import annotations

import hashlib
import importlib.util
import io
import json
import os
import pathlib
import sys
import tempfile
import unittest
import urllib.error
from unittest import mock

_spec = importlib.util.spec_from_file_location("du", pathlib.Path(__file__).resolve().parent.parent / "docker-update.py")
du = importlib.util.module_from_spec(_spec)
sys.modules["du"] = du
_spec.loader.exec_module(du)

DAY = 86400


def manifest(n: int) -> bytes:
    """Faux manifeste d'index ; son hachage est le digest que le « registre » annonce."""
    return f'{{"schemaVersion":2,"n":{n}}}\n'.encode()


def dg(n: int) -> str:
    return "sha256:" + hashlib.sha256(manifest(n)).hexdigest()


class Clock:
    def __init__(self, t: float = 1_000_000):
        self.t = t

    def __call__(self) -> float:
        return self.t

    def days(self, n: float) -> None:
        self.t += n * DAY


class World:
    """Un hôte Docker minimal : suffisant pour jouer les scénarios de mise à jour."""

    def __init__(self):
        self.registry: dict[str, bytes] = {}
        self.images: dict[str, list[str]] = {}       # id -> ["repo@digest"]
        self.by_ref_digest: dict[str, str] = {}      # "repo@digest" -> id
        self.tags: dict[str, str] = {}               # ref -> id
        self.containers: dict[tuple[str, str], dict] = {}
        self.compose: dict[str, dict[str, str]] = {}
        self.calls: list[list[str]] = []
        self.ups: list[tuple[str, str]] = []
        self.hooks: list[list[str]] = []
        self.fail_up: set[str] = set()
        self.restarting: set[str] = set()
        self.fail_pull: set[str] = set()
        self.fail_registry: set[str] = set()
        self.fail_hook: set[str] = set()
        self.dry_run = False
        self.log = lambda m: None
        self.secret_in_config = "hunter2"
        self.labels: dict[str, dict] = {}            # id d'image -> étiquettes
        self.digest_versions: dict[str, str] = {}    # digest -> version annoncée par l'image candidate

    def add(self, name: str, ddir: str, service: str, ref: str, local: int, exists: bool = True,
            digests: bool = True, version: str = "") -> str:
        repo, tag, _ = du.split_ref(ref)
        iid = f"id-{name}-old"
        self.images[iid] = [f"{repo}@{dg(local)}"] if digests else []
        self.tags[ref] = iid
        self.compose.setdefault(ddir, {})[service] = ref
        self.registry[f"{repo}:{tag}"] = manifest(local)
        if version:
            self.labels[iid] = {"org.opencontainers.image.version": version}
        if exists:
            self.containers[(ddir, service)] = {"id": f"cid-{service}", "image_id": iid, "status": "running", "health": ""}
        return iid

    def run_bytes(self, argv, timeout=60):
        self.calls.append(list(argv))
        assert argv[:4] == ["docker", "buildx", "imagetools", "inspect"] and argv[-1] == "--raw", argv
        full = argv[4]
        if full in self.fail_registry:
            return 1, b"", "unauthorized"
        return 0, self.registry[full], ""

    def run(self, argv, mutating=False, timeout=900):
        if mutating and self.dry_run:
            return du.Result(0)
        self.calls.append(list(argv))
        R = du.Result
        if argv[:2] == ["docker", "compose"]:
            ddir = argv[argv.index("--project-directory") + 1]
            sub = [a for a in argv[2:] if a in ("ps", "config", "up")]
            if sub[0] == "ps":
                key = (ddir, argv[-1])
                return R(0, self.containers[key]["id"] + "\n" if key in self.containers else "")
            if sub[0] == "config":
                svcs = {s: {"image": r, "environment": {"TOKEN": self.secret_in_config}} for s, r in self.compose[ddir].items()}
                return R(0, json.dumps({"services": svcs}))
            service = argv[-1]
            ref = self.compose[ddir][service]
            iid = self.tags[ref]
            self.ups.append((service, iid))
            if iid in self.fail_up:
                return R(1, "", "service unhealthy")
            health = ""
            self.containers[(ddir, service)] = {"id": f"cid-{service}", "image_id": iid,
                                                "status": "restarting" if iid in self.restarting else "running",
                                                "health": health}
            return R(0)
        if argv[:2] == ["docker", "inspect"]:
            cid = argv[-1]
            c = next(v for v in self.containers.values() if v["id"] == cid)
            if "State.Status" in argv[3]:
                return R(0, f"{c['status']}|{c['health']}\n")
            return R(0, c["image_id"] + "\n")
        if argv[:3] == ["docker", "image", "inspect"]:
            if "Config.Labels" in argv[4]:
                iid = self.by_ref_digest.get(argv[-1], argv[-1])
                return R(0, json.dumps(self.labels.get(iid, {})))
            return R(0, json.dumps(self.images.get(argv[-1], [])))
        if argv[:2] == ["regctl", "image"]:
            full = argv[3]
            if full in self.fail_registry:
                return R(1, "", "unauthorized")
            raw = self.registry[full]
            return R(0, "sha256:" + hashlib.sha256(raw).hexdigest() + "\n" if raw else "\n")
        if argv[:2] == ["docker", "pull"]:
            target = argv[2]
            if target.split("@", 1)[1] in self.fail_pull:
                return R(1, "", "pull failed")
            iid = "id-" + target.split("@", 1)[1][7:19]
            self.images[iid] = [target]
            self.by_ref_digest[target] = iid
            version = self.digest_versions.get(target.split("@", 1)[1], "")
            if version:
                self.labels[iid] = {"org.opencontainers.image.version": version}
            return R(0)
        if argv[:2] == ["docker", "tag"]:
            src = argv[2]
            iid = self.by_ref_digest.get(src, src)
            self.tags[argv[3]] = iid
            return R(0)
        if argv[0].startswith("/"):
            self.hooks.append(list(argv))
            return R(1 if argv[0] in self.fail_hook else 0)
        raise AssertionError(f"commande inattendue : {argv}")


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.logs: list[str] = []
        self.posts: list[tuple[str, dict, dict]] = []
        self.clock = Clock()
        self.sleeps: list[float] = []
        self.world = World()
        self.world.log = self.logs.append
        self.minutes = 180
        self.http_codes: dict[str, int] = {}
        self.url_file = os.path.join(self.tmp.name, "notify_url")
        with open(self.url_file, "w") as f:
            f.write("https://discord.test/api/webhooks/1/SECRET-TOKEN\n")
        self.state_path = os.path.join(self.tmp.name, "state", "state.json")
        self.gh: dict[str, tuple] = {}              # "owner/repo" -> (code, corps, etag)
        self.gh_calls: list[tuple[str, str, str]] = []

    def _gh_get(self, url, token, etag):
        self.gh_calls.append((url, token, etag))
        repo = url.split("/repos/", 1)[1].split("/security-advisories")[0]
        code, body, new_etag = self.gh.get(repo, (404, None, ""))
        if code == 200 and etag and etag == new_etag:
            return 304, None, new_etag
        return code, body, new_etag

    def make(self, services: list[dict], settings: dict | None = None, tool="buildx", dry_run=False,
             persist=True, fmt="discord") -> "du.Updater":
        st = {"state_file": self.state_path, "lock_file": os.path.join(self.tmp.name, "lock"),
              "notify_url_file": self.url_file, "notify_format": fmt, "registry_tool": tool,
              "settle_seconds": 7}
        st.update(settings or {})
        cfg = du.parse_config({"settings": st, "service": services})
        self.world.dry_run = dry_run
        notifier = du.Notifier(self.url_file, fmt, self.logs.append,
                               post=lambda u, b, h: self.posts.append((u, json.loads(b) if fmt != "ntfy" else b, h)),
                               dry_run=dry_run)
        return du.Updater(cfg, self.world, notifier, du.State(self.state_path), now=self.clock,
                          sleep=self.sleeps.append, log=self.logs.append,
                          http=lambda url: self.http_codes.get(url, 0), minutes=lambda: self.minutes,
                          persist=persist, gh_get=self._gh_get)

    @staticmethod
    def svc(name="traefik", ddir="/stack/traefik", service=None, **kw) -> dict:
        d = {"name": name, "compose_dir": ddir, "service": service or name, "mode": "auto"}
        d.update(kw)
        return d

    def traefik(self, local=1) -> str:
        return self.world.add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", local)

    def new_remote(self, ref_full="traefik:v3.6", n=2):
        self.world.registry[ref_full] = manifest(n)

    def cmds(self, prefix) -> list[list[str]]:
        return [c for c in self.world.calls if c[:len(prefix)] == prefix]

    def logtext(self) -> str:
        return "\n".join(self.logs)


# ── Outils de base ──────────────────────────────────────────────────────────

class TestHelpers(unittest.TestCase):
    def test_parse_duration(self):
        self.assertEqual(du.parse_duration("0"), 0)
        self.assertEqual(du.parse_duration("30m"), 1800)
        self.assertEqual(du.parse_duration("6h"), 21600)
        self.assertEqual(du.parse_duration("3d"), 3 * DAY)
        self.assertEqual(du.parse_duration("1w"), 7 * DAY)
        for bad in ("", "3", "d", "3 jours", "-1d", "1.5d", "3D"):
            with self.assertRaises(du.ConfigError, msg=bad):
                du.parse_duration(bad)

    def test_split_ref(self):
        self.assertEqual(du.split_ref("traefik:v3.6"), ("traefik", "v3.6", None))
        self.assertEqual(du.split_ref("traefik"), ("traefik", "latest", None))
        self.assertEqual(du.split_ref("ghcr.io/org/app:main"), ("ghcr.io/org/app", "main", None))
        self.assertEqual(du.split_ref("localhost:5000/app"), ("localhost:5000/app", "latest", None))
        self.assertEqual(du.split_ref("localhost:5000/app:1.2"), ("localhost:5000/app", "1.2", None))
        self.assertEqual(du.split_ref("app:1@sha256:abc"), ("app", "1", "sha256:abc"))

    def test_windows(self):
        self.assertIsNone(du.parse_window(""))
        w = du.parse_window("02:00-05:00")
        self.assertTrue(du.in_window(w, 120))
        self.assertTrue(du.in_window(w, 299))
        self.assertFalse(du.in_window(w, 300))
        self.assertFalse(du.in_window(w, 60))
        night = du.parse_window("22:00-04:00")
        self.assertTrue(du.in_window(night, 23 * 60))
        self.assertTrue(du.in_window(night, 3 * 60))
        self.assertFalse(du.in_window(night, 12 * 60))
        self.assertTrue(du.in_window(None, 999))
        for bad in ("2-5", "25:00-26:00", "02:00-02:00", "02:61-03:00", "nuit"):
            with self.assertRaises(du.ConfigError, msg=bad):
                du.parse_window(bad)


class TestConfig(unittest.TestCase):
    def base(self, **over):
        s = {"name": "a", "compose_dir": "/x", "service": "a"}
        s.update(over)
        return {"service": [s]}

    def test_valid_and_defaults(self):
        cfg = du.parse_config({"settings": {"default_cooldown": "2d"}, "service": [
            {"name": "a", "compose_dir": "/x", "service": "web", "compose_files": ["c.yml", "/abs/o.yml"]}]})
        s = cfg.services[0]
        self.assertEqual(s.cooldown, 2 * DAY)
        self.assertEqual(s.mode, "notify")  # le plus prudent par défaut
        self.assertTrue(s.rollback)
        self.assertEqual(s.compose_files, ["/x/c.yml", "/abs/o.yml"])

    def test_rejections(self):
        bad = [
            ({"settings": {"nope": 1}}, "inconnue"),
            (self.base(mode="fast"), "mode"),
            (self.base(compose_dir="x"), "absolu"),
            (self.base(name="a b"), "nom invalide"),
            (self.base(service="x;rm"), "invalide"),
            (self.base(typo=1), "inconnue"),
            (self.base(cooldown="3"), "durée"),
            (self.base(pre_hook=[["relatif"]]), "chemin absolu"),
            (self.base(pre_hook=["/bin/true"]), "pre_hook"),
            (self.base(window="nuit"), "fenêtre"),
            (self.base(smoke_url="ftp://x"), "http"),
            (self.base(smoke_url="https://x"), "smoke_expect"),
            (self.base(smoke_expect=[200]), "sans smoke_url"),
            (self.base(health_timeout=0), "health_timeout"),
            ({"settings": {"registry_tool": "curl"}}, "registry_tool"),
            ({"settings": {"notify_format": "sms"}}, "notify_format"),
        ]
        for data, word in bad:
            with self.assertRaises(du.ConfigError, msg=str(data)) as cm:
                du.parse_config(data)
            self.assertIn(word, str(cm.exception))

    def test_duplicates_and_groups(self):
        one = {"name": "a", "compose_dir": "/x", "service": "a"}
        with self.assertRaises(du.ConfigError):
            du.parse_config({"service": [one, dict(one)]})
        with self.assertRaises(du.ConfigError):  # groupe incohérent
            du.parse_config({"service": [dict(one, group="g", mode="auto"), dict(one, name="b", group="g", mode="notify")]})
        with self.assertRaises(du.ConfigError):  # groupe = nom d'un service
            du.parse_config({"service": [dict(one, name="g"), dict(one, name="b", group="g")]})

    def test_example_config_is_valid(self):
        path = pathlib.Path(__file__).resolve().parent.parent / "examples" / "updates.example.toml"
        cfg = du.load_config(str(path))
        self.assertGreaterEqual(len(cfg.services), 10)
        groups = {s.group for s in cfg.services if s.group}
        self.assertEqual(groups, {"tirelire"})
        tirelire = [s for s in cfg.services if s.group == "tirelire"]
        self.assertFalse(any(s.rollback for s in tirelire), "pas de retour arrière auto sur une base migrée")
        self.assertEqual(sum(1 for s in tirelire if s.pre_hook), 1, "le crochet n'est déclaré qu'une fois")
        for s in cfg.services:
            self.assertIn(s.mode, ("auto", "notify"))

    def test_load_config_errors(self):
        with self.assertRaises(du.ConfigError):
            du.load_config("/nonexistent/x.toml")
        with tempfile.NamedTemporaryFile("w", suffix=".toml", delete=False) as f:
            f.write("ceci = n'est pas du toml [")
        self.addCleanup(os.unlink, f.name)
        with self.assertRaises(du.ConfigError):
            du.load_config(f.name)

    def test_trusted_file(self):
        with tempfile.NamedTemporaryFile(delete=False) as f:
            pass
        self.addCleanup(os.unlink, f.name)
        fake = mock.Mock(st_uid=1000, st_mode=0o100644)
        with mock.patch("os.geteuid", return_value=0), mock.patch("os.stat", return_value=fake):
            with self.assertRaises(du.ConfigError):
                du.check_trusted_file(f.name)  # pas à root
        fake = mock.Mock(st_uid=0, st_mode=0o100666)
        with mock.patch("os.geteuid", return_value=0), mock.patch("os.stat", return_value=fake):
            with self.assertRaises(du.ConfigError):
                du.check_trusted_file(f.name)  # modifiable par tous
        fake = mock.Mock(st_uid=0, st_mode=0o100644)
        with mock.patch("os.geteuid", return_value=0), mock.patch("os.stat", return_value=fake):
            du.check_trusted_file(f.name)      # OK
        with mock.patch("os.geteuid", return_value=1000):
            du.check_trusted_file(f.name)      # pas root : pas de contrainte


# ── Cycle de mise à jour ────────────────────────────────────────────────────

class TestCycle(Base):
    def test_up_to_date_does_nothing(self):
        self.traefik()
        u = self.make([self.svc()])
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.world.ups, [])
        self.assertEqual(u.state.images, {})
        self.assertIn("traefik: à jour", self.logtext())

    def test_new_digest_is_deferred_then_applied(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()])
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [], "pas de mise à jour avant la fin du délai")
        self.assertEqual(u.state.images["traefik:v3.6"]["first_seen"], int(self.clock.t))
        self.assertIn("différée", self.logtext())
        self.assertEqual(self.posts, [], "aucune notification pour une simple différée")
        self.clock.days(2.9)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.clock.days(0.2)  # 3,1 jours
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(2)[7:19])
        # tirage par digest exact, pas par étiquette
        self.assertEqual(self.cmds(["docker", "pull"]), [["docker", "pull", f"traefik@{dg(2)}"]])
        self.assertEqual(self.cmds(["docker", "tag"])[0], ["docker", "tag", f"traefik@{dg(2)}", "traefik:v3.6"])
        self.assertTrue(any("Mise à jour réussie" in p[1]["content"] for p in self.posts))

    def test_order_of_operations(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(pre_hook=[["/usr/local/sbin/avant"]])], settings={"default_cooldown": "0"})
        u.cycle(apply=True)
        kinds = []
        for c in self.world.calls:
            if c[0] == "/usr/local/sbin/avant":
                kinds.append("hook")
            elif c[:2] == ["docker", "pull"]:
                kinds.append("pull")
            elif c[:2] == ["docker", "tag"]:
                kinds.append("tag")
            elif "up" in c and c[:2] == ["docker", "compose"]:
                kinds.append("up")
        self.assertEqual(kinds, ["hook", "pull", "tag", "up"])
        up = next(c for c in self.world.calls if c[:2] == ["docker", "compose"] and "up" in c)
        for flag in ("--no-deps", "--wait", "never"):
            self.assertIn(flag, up)
        self.assertEqual(self.sleeps, [7], "attente de stabilisation après démarrage")

    def test_digest_moving_resets_timer(self):
        self.traefik()
        self.new_remote(n=2)
        u = self.make([self.svc()])
        u.cycle(apply=True)
        self.clock.days(2)
        self.new_remote(n=3)          # l'étiquette bouge avant la fin du délai
        u.cycle(apply=True)
        self.clock.days(1.5)          # 3,5 j depuis D2, 1,5 j depuis D3
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.clock.days(1.6)          # 3,1 j depuis D3
        u.cycle(apply=True)
        self.assertEqual(self.cmds(["docker", "pull"]), [["docker", "pull", f"traefik@{dg(3)}"]])

    def test_tag_moving_back_clears_state(self):
        self.traefik()
        self.new_remote(n=2)
        u = self.make([self.svc()])
        u.cycle(apply=True)
        self.assertIn("traefik:v3.6", u.state.images)
        self.new_remote(n=1)          # retour au digest en cours
        u.cycle(apply=True)
        self.assertEqual(u.state.images, {})

    def test_state_persists_between_runs(self):
        self.traefik()
        self.new_remote()
        self.make([self.svc()]).cycle(apply=True)
        first = int(self.clock.t)
        self.clock.days(4)
        u2 = self.make([self.svc()])           # nouveau processus : recharge l'état du disque
        self.assertEqual(u2.state.images["traefik:v3.6"]["first_seen"], first)
        u2.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1)

    def test_check_never_applies_or_notifies(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()], settings={"default_cooldown": "0"})
        u.cycle(apply=False)
        self.assertEqual(self.world.ups, [])
        self.assertEqual(self.cmds(["docker", "pull"]), [])
        self.assertEqual(self.posts, [])
        self.assertTrue(os.path.exists(self.state_path), "check mémorise ce qu'il a vu")


class TestFailures(Base):
    def go(self, **kw):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(**kw)], settings={"default_cooldown": "0"})
        return u

    def test_failed_start_rolls_back(self):
        u = self.go()
        self.world.fail_up.add("id-" + dg(2)[7:19])
        self.assertEqual(u.cycle(apply=True), 1)
        c = self.world.containers[("/stack/traefik", "traefik")]
        self.assertEqual(c["image_id"], "id-traefik-old", "retour à l'image précédente")
        self.assertEqual(self.world.tags["traefik:v3.6"], "id-traefik-old")
        self.assertTrue(u.state.images["traefik:v3.6"]["failed"])
        msg = " ".join(p[1]["content"] for p in self.posts)
        self.assertIn("retour arrière", msg)

    def test_failed_digest_is_not_retried_but_new_one_is(self):
        u = self.go()
        self.world.fail_up.add("id-" + dg(2)[7:19])
        u.cycle(apply=True)
        ups = len(self.world.ups)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), ups, "même digest en échec : pas de nouvelle tentative")
        self.assertIn("déjà tentée", self.logtext())
        self.new_remote(n=4)          # nouvelle version publiée
        u.cycle(apply=True)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(4)[7:19])

    def test_rollback_disabled(self):
        u = self.go(rollback=False)
        self.world.fail_up.add("id-" + dg(2)[7:19])
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(self.world.tags["traefik:v3.6"], "id-" + dg(2)[7:19], "aucun retour arrière automatique")
        self.assertIn("DÉSACTIVÉ", " ".join(p[1]["content"] for p in self.posts))

    def test_rollback_itself_fails(self):
        u = self.go()
        self.world.fail_up.update({"id-" + dg(2)[7:19], "id-traefik-old"})
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertIn("URGENT", " ".join(p[1]["content"] for p in self.posts))

    def test_restarting_after_up_counts_as_failure(self):
        u = self.go()
        self.world.restarting.add("id-" + dg(2)[7:19])  # up réussit mais le conteneur boucle
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-traefik-old")

    def test_hook_failure_aborts_before_any_change(self):
        u = self.go(pre_hook=[["/usr/local/sbin/avant"]])
        self.world.fail_hook.add("/usr/local/sbin/avant")
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(self.cmds(["docker", "pull"]), [])
        self.assertEqual(self.world.ups, [])
        self.assertEqual(self.world.tags["traefik:v3.6"], "id-traefik-old")
        self.assertIn("annulée", " ".join(p[1]["content"] for p in self.posts))

    def test_pull_failure_aborts_before_any_change(self):
        u = self.go()
        self.world.fail_pull.add(dg(2))
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(self.cmds(["docker", "tag"]), [])
        self.assertEqual(self.world.ups, [])
        # un échec réseau n'est pas marqué « image défaillante » : on réessaiera
        self.assertFalse(u.state.images["traefik:v3.6"].get("failed"))

    def test_registry_error_is_reported_once(self):
        self.traefik()
        self.world.fail_registry.add("traefik:v3.6")
        u = self.make([self.svc()])
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(len(self.posts), 1)
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.assertEqual(len(self.posts), 1, "même erreur : pas de répétition")
        self.world.fail_registry.clear()
        u.cycle(apply=True)
        self.assertEqual(u.state.data["errors"], {})
        self.world.fail_registry.add("traefik:v3.6")
        u.cycle(apply=True)
        self.assertEqual(len(self.posts), 2, "l'erreur revient après résolution : on prévient de nouveau")

    def test_smoke_test(self):
        self.traefik()
        self.new_remote()
        url = "https://exemple.test/"
        u = self.make([self.svc(smoke_url=url, smoke_expect=[302, 401])], settings={"default_cooldown": "0"})
        self.http_codes[url] = 0  # le site ne répond pas après la mise à jour
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-traefik-old")
        self.assertIn("test de fumée", self.logtext())
        self.assertGreaterEqual(self.sleeps.count(5), 2, "trois essais espacés")

    def test_smoke_test_passes(self):
        self.traefik()
        self.new_remote()
        url = "https://exemple.test/"
        self.http_codes[url] = 302
        u = self.make([self.svc(smoke_url=url, smoke_expect=[302, 401])], settings={"default_cooldown": "0"})
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(2)[7:19])


class TestModesAndPolicies(Base):
    def test_notify_mode_notifies_once_and_never_applies(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(mode="notify", cooldown="1d")])
        u.cycle(apply=True)
        self.assertEqual(self.posts, [])
        self.clock.days(1.1)
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.assertEqual(len(self.posts), 1)
        self.assertEqual(self.world.ups, [])
        self.assertIn("docker-update now traefik --yes", self.posts[0][1]["content"])

    def test_manual_mode_is_silent(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(mode="manual", cooldown="0")])
        u.cycle(apply=True)
        self.assertEqual((self.posts, self.world.ups), ([], []))

    def test_window(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(window="02:00-05:00")], settings={"default_cooldown": "0"})
        self.minutes = 14 * 60
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("hors de la fenêtre", self.logtext())
        self.minutes = 3 * 60
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1)

    def test_global_window_and_override(self):
        self.traefik()
        self.new_remote()
        self.minutes = 14 * 60
        # sans clé « window » : la fenêtre globale s'applique
        u = self.make([self.svc()], settings={"default_cooldown": "0", "window": "02:00-05:00"})
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [], "la fenêtre globale s'applique")
        # « window = "" » explicite : ce service n'est limité par aucune fenêtre
        u = self.make([self.svc(window="")], settings={"default_cooldown": "0", "window": "02:00-05:00"})
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1, "une fenêtre vide désactive la fenêtre globale")
        # une fenêtre propre remplace la globale
        cfg = du.parse_config({"settings": {"window": "02:00-05:00"}, "service": [
            {"name": "a", "compose_dir": "/x", "service": "a", "window": "10:00-11:00"}]})
        self.assertEqual(cfg.services[0].window, (600, 660))

    def test_hold(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()], settings={"default_cooldown": "0"})
        u.hold(["traefik"], True)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("en pause", self.logtext())
        with self.assertRaises(du.UpdateError):
            u.now_cmd(["traefik"], yes=True)
        with self.assertRaises(du.UpdateError):
            u.hold(["inconnu"], True)
        u.hold(["traefik"], False)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1)

    def test_hold_survives_restart(self):
        self.traefik()
        self.make([self.svc()]).hold(["traefik"], True)
        self.assertEqual(self.make([self.svc()]).state.data["holds"], ["traefik"])

    def test_skips(self):
        self.world.add("pinned", "/s/p", "p", "app:1@sha256:" + "a" * 64, 1)
        self.world.add("absent", "/s/a", "a", "app2:1", 1, exists=False)
        self.world.add("local", "/s/l", "l", "app3:1", 1, digests=False)
        self.world.registry["app3:1"] = manifest(9)
        u = self.make([self.svc("pinned", "/s/p", "p"), self.svc("absent", "/s/a", "a"), self.svc("local", "/s/l", "l")])
        self.assertEqual(u.cycle(apply=True), 0)
        text = self.logtext()
        self.assertIn("épinglée", text)
        self.assertIn("conteneur absent", text)
        self.assertIn("sans digest", text)
        self.assertEqual(self.world.ups, [])

    def test_floating_tag_warning(self):
        self.world.add("x", "/s/x", "x", "app:latest", 1)
        u = self.make([self.svc("x", "/s/x", "x")])
        u.cycle(apply=True)
        self.assertIn("flottante", self.logtext())

    def test_regctl_backend(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()], tool="regctl", settings={"default_cooldown": "0"})
        u.cycle(apply=True)
        self.assertEqual(self.cmds(["regctl", "image", "digest"]), [["regctl", "image", "digest", "traefik:v3.6"]])
        self.assertEqual(len(self.world.ups), 1)

    def test_bad_manifest_output_is_an_error(self):
        for junk in (b"", b"<html>proxy error</html>", b"x" * (du.MAX_MANIFEST + 1)):
            with self.subTest(len(junk)):
                self.world = World()
                self.world.log = self.logs.append
                self.traefik()
                self.world.registry["traefik:v3.6"] = junk
                u = self.make([self.svc()])
                self.assertEqual(u.cycle(apply=True), 1)
                self.assertEqual(self.world.ups, [])
                self.assertEqual(u.state.images, {}, "aucun état créé sur une réponse invalide")

    def test_regctl_bad_output_is_an_error(self):
        self.traefik()
        self.world.registry["traefik:v3.6"] = b""   # le faux regctl répond « \n »
        u = self.make([self.svc()], tool="regctl")
        self.assertEqual(u.cycle(apply=True), 1)

    def test_buildx_reads_the_raw_manifest_and_not_a_template(self):
        self.traefik()
        u = self.make([self.svc()])
        u.cycle(apply=True)
        call = next(c for c in self.world.calls if "imagetools" in c)
        self.assertEqual(call, ["docker", "buildx", "imagetools", "inspect", "traefik:v3.6", "--raw"])
        self.assertNotIn("--format", call, "pas de gabarit : il ferait télécharger les attestations")


class TestGroups(Base):
    def stack(self, n=3):
        for s in ("migrate", "web", "admin")[:n]:
            self.world.add(s, "/stack/tirelire", s, "ghcr.io/org/tirelire:main", 1)
        self.world.registry["ghcr.io/org/tirelire:main"] = manifest(2)
        return [self.svc(s, "/stack/tirelire", s, group="tirelire", rollback=True)
                for s in ("migrate", "web", "admin")[:n]]

    def test_group_updates_in_order_with_single_pull(self):
        u = self.make(self.stack(), settings={"default_cooldown": "0"})
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual([s for s, _ in self.world.ups], ["migrate", "web", "admin"])
        self.assertEqual(len(self.cmds(["docker", "pull"])), 1)
        self.assertEqual(len(self.posts), 1, "un seul message pour tout le groupe")

    def test_group_rolls_back_everything_touched_in_reverse(self):
        u = self.make(self.stack(), settings={"default_cooldown": "0"})
        self.world.fail_up.add("id-" + dg(2)[7:19])
        self.world.fail_up.discard("id-" + dg(2)[7:19])
        # échec seulement sur admin (3e) : on le simule via une image « qui boucle » appliquée à admin uniquement
        orig = self.world.run

        def run(argv, mutating=False, timeout=900):
            if argv[:2] == ["docker", "compose"] and "up" in argv and argv[-1] == "admin" \
                    and self.world.tags["ghcr.io/org/tirelire:main"] == "id-" + dg(2)[7:19]:
                self.world.ups.append(("admin", "échec"))
                return du.Result(1, "", "unhealthy")
            return orig(argv, mutating, timeout)

        u.run = type("R", (), {"run": staticmethod(run), "run_bytes": staticmethod(self.world.run_bytes), "dry_run": False})()
        self.assertEqual(u.cycle(apply=True), 1)
        for s in ("migrate", "web", "admin"):
            self.assertEqual(self.world.containers[("/stack/tirelire", s)]["image_id"], "id-" + s + "-old", s)
        order = [s for s, _ in self.world.ups]
        self.assertEqual(order[:3], ["migrate", "web", "admin"])
        self.assertEqual(order[3:], ["admin", "web", "migrate"], "retour arrière en ordre inverse")

    def test_group_without_rollback_leaves_state_and_alerts(self):
        stack = self.stack()
        for s in stack:
            s["rollback"] = False
        u = self.make(stack, settings={"default_cooldown": "0"})
        self.world.fail_up.add("id-" + dg(2)[7:19])
        self.assertEqual(u.cycle(apply=True), 1)
        self.assertEqual([s for s, _ in self.world.ups], ["migrate"], "on s'arrête au premier échec")
        self.assertIn("DÉSACTIVÉ", " ".join(p[1]["content"] for p in self.posts))

    def test_shared_ref_keeps_timer_when_only_one_member_is_up_to_date(self):
        stack = self.stack(2)
        u = self.make(stack)
        u.cycle(apply=True)
        t0 = u.state.images["ghcr.io/org/tirelire:main"]["first_seen"]
        # migrate passe à jour tout seul, web reste en retard
        self.world.images["id-migrate-old"] = [f"ghcr.io/org/tirelire@{dg(2)}"]
        self.clock.days(1)
        u.cycle(apply=True)
        self.assertEqual(u.state.images["ghcr.io/org/tirelire:main"]["first_seen"], t0)

    def test_now_targets_group_by_name(self):
        u = self.make(self.stack(), settings={"default_cooldown": "30d"})
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertEqual(u.now_cmd(["tirelire"], yes=True), 0)
        self.assertEqual([s for s, _ in self.world.ups], ["migrate", "web", "admin"])


class TestNow(Base):
    def test_now_bypasses_cooldown_window_and_mode(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(mode="notify", window="02:00-05:00", cooldown="30d")])
        self.minutes = 15 * 60
        self.assertEqual(u.now_cmd(["traefik"], yes=False), 0)
        self.assertEqual(self.world.ups, [], "sans --yes : simulation")
        self.assertIn("--yes", self.logtext())
        self.assertEqual(u.now_cmd(["traefik"], yes=True), 0)
        self.assertEqual(len(self.world.ups), 1)

    def test_now_unknown_name(self):
        self.traefik()
        u = self.make([self.svc()])
        with self.assertRaises(du.UpdateError):
            u.now_cmd(["nimporte"], yes=True)

    def test_now_nothing_to_do(self):
        self.traefik()
        u = self.make([self.svc()])
        self.assertEqual(u.now_cmd(["traefik"], yes=True), 0)
        self.assertIn("rien à mettre à jour", self.logtext())

    def test_now_still_retries_a_failed_digest(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()], settings={"default_cooldown": "0"})
        self.world.fail_up.add("id-" + dg(2)[7:19])
        u.cycle(apply=True)
        self.world.fail_up.clear()
        self.assertEqual(u.now_cmd(["traefik"], yes=True), 0)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(2)[7:19])


class TestDryRun(Base):
    def test_dry_run_changes_nothing(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(pre_hook=[["/usr/local/sbin/avant"]])], settings={"default_cooldown": "0"},
                      dry_run=True, persist=False)
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.cmds(["docker", "pull"]), [])
        self.assertEqual(self.cmds(["docker", "tag"]), [])
        self.assertEqual(self.world.ups, [])
        self.assertEqual(self.world.hooks, [])
        self.assertEqual(self.posts, [])
        self.assertFalse(os.path.exists(self.state_path), "la simulation n'écrit pas l'état")
        self.assertIn("[simulation]", self.logtext())

    def test_real_runner_dry_run_only_prints(self):
        out: list[str] = []
        r = du.Runner(dry_run=True, log=out.append)
        res = r.run(["/definitely/not/a/command", "x y"], mutating=True)
        self.assertEqual(res.rc, 0)
        self.assertIn("[simulation]", out[0])
        self.assertIn("'x y'", out[0])


class TestInfra(unittest.TestCase):
    def test_runner(self):
        r = du.Runner()
        self.assertEqual(r.run([sys.executable, "-c", "print('ok')"]).out.strip(), "ok")
        self.assertEqual(r.run([sys.executable, "-c", "import sys; sys.exit(3)"]).rc, 3)
        self.assertEqual(r.run(["/nope/nope"]).rc, 127)
        self.assertEqual(r.run([sys.executable, "-c", "import time; time.sleep(5)"], timeout=1).rc, 124)

    def test_runner_bytes_keeps_exact_bytes(self):
        r = du.Runner()
        rc, out, err = r.run_bytes([sys.executable, "-c",
                                    "import sys; sys.stdout.buffer.write(b'{\\r\\n}\\r\\n\\x00')"])
        self.assertEqual((rc, out), (0, b"{\r\n}\r\n\x00"), "aucune traduction de fin de ligne")
        self.assertEqual(r.run_bytes(["/nope/nope"])[0], 127)
        self.assertEqual(r.run_bytes([sys.executable, "-c", "import time; time.sleep(5)"], timeout=1)[0], 124)

    def test_state_roundtrip_and_corruption(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "sub", "state.json")
            s = du.State(p)
            s.put("a:1", {"digest": dg(1), "first_seen": 5})
            s.data["holds"].append("x")
            s.save()
            self.assertEqual(oct(os.stat(p).st_mode & 0o777), "0o600")
            self.assertEqual([f for f in os.listdir(os.path.dirname(p)) if f.startswith(".state-")], [])
            s2 = du.State(p)
            self.assertEqual(s2.get("a:1")["first_seen"], 5)
            self.assertEqual(s2.data["holds"], ["x"])
            for bad in ("{pas du json", "[1,2]", '{"images": []}'):
                with open(p, "w") as f:
                    f.write(bad)
                with self.assertRaises(du.UpdateError, msg=bad):
                    du.State(p)

    def test_notifier_discord_payload_and_secret_never_logged(self):
        with tempfile.TemporaryDirectory() as d:
            uf = os.path.join(d, "u")
            with open(uf, "w") as f:
                f.write("https://discord.test/api/webhooks/1/TOP-SECRET\n")
            sent = []
            logs = []
            n = du.Notifier(uf, "discord", logs.append, post=lambda u, b, h: sent.append((u, json.loads(b), h)))
            n.send("Titre", "x" * 5000, "error")
            url, body, headers = sent[0]
            self.assertEqual(body["allowed_mentions"], {"parse": []}, "aucune mention possible")
            self.assertLessEqual(len(body["content"]), du.DISCORD_MAX)
            self.assertEqual(headers["Content-Type"], "application/json")
            self.assertTrue(headers["User-Agent"].startswith("docker-update"))

            def boom(u, b, h):
                raise urllib.error.URLError(f"échec vers {u}")
            du.Notifier(uf, "discord", logs.append, post=boom).send("t", "m")
            self.assertTrue(any("non envoyée" in l for l in logs))
            self.assertFalse(any("TOP-SECRET" in l for l in logs), "l'URL ne doit jamais apparaître")

    def test_notifier_without_url_and_other_formats(self):
        sent = []
        du.Notifier("", "discord", print, post=lambda *a: sent.append(a)).send("t", "m")
        du.Notifier("/nonexistent", "discord", print, post=lambda *a: sent.append(a)).send("t", "m")
        self.assertEqual(sent, [])
        with tempfile.TemporaryDirectory() as d:
            uf = os.path.join(d, "u")
            with open(uf, "w") as f:
                f.write("https://x.test/t")
            du.Notifier(uf, "ntfy", print, post=lambda u, b, h: sent.append((b, h))).send("Titre é", "corps", "error")
            du.Notifier(uf, "json", print, post=lambda u, b, h: sent.append((json.loads(b), h))).send("t", "m")
        self.assertEqual(sent[0][0], b"corps")
        self.assertEqual(sent[0][1]["Priority"], "high")
        self.assertEqual(sent[1][0]["level"], "info")

    def test_http_status_unreachable(self):
        self.assertEqual(du.http_status("http://127.0.0.1:9/", timeout=1), 0)

    def test_validate(self):
        with tempfile.TemporaryDirectory() as d:
            cfg = du.parse_config({"settings": {"notify_url_file": os.path.join(d, "absent")}, "service": [
                {"name": "a", "compose_dir": d, "service": "a", "pre_hook": [["/nonexistent/hook"]]},
                {"name": "b", "compose_dir": "/nonexistent/dir", "service": "b"}]})
            out: list[str] = []
            rc = du.validate(cfg, out.append)
            text = "\n".join(out)
            self.assertEqual(rc, 1)
            self.assertIn("introuvable", text)
            self.assertIn("non exécutable", text)
            self.assertIn("AVERTISSEMENT", text)


class TestSecurityProperties(Base):
    def test_compose_config_content_is_never_logged(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc()], settings={"default_cooldown": "0"})
        u.cycle(apply=True)
        u.cycle(apply=False)
        blob = self.logtext() + json.dumps([p[1] for p in self.posts])
        self.assertNotIn(self.world.secret_in_config, blob)
        self.assertNotIn("SECRET-TOKEN", blob)

    def test_commands_never_use_a_shell(self):
        self.traefik()
        self.new_remote()
        u = self.make([self.svc(pre_hook=[["/usr/local/sbin/avant", "arg; rm -rf /"]])], settings={"default_cooldown": "0"})
        u.cycle(apply=True)
        for c in self.world.calls:
            self.assertIsInstance(c, list)
        self.assertIn(["/usr/local/sbin/avant", "arg; rm -rf /"], self.world.hooks, "l'argument reste un seul argument")

    def test_pull_is_by_digest_of_the_evaluated_image(self):
        self.traefik()
        self.new_remote(n=2)
        u = self.make([self.svc()], settings={"default_cooldown": "0"})
        orig = self.world.run

        def run(argv, mutating=False, timeout=900):
            if argv[:2] == ["docker", "pull"]:
                self.world.registry["traefik:v3.6"] = manifest(99)  # l'étiquette bouge juste avant le tirage
            return orig(argv, mutating, timeout)
        u.run = type("R", (), {"run": staticmethod(run), "run_bytes": staticmethod(self.world.run_bytes), "dry_run": False})()
        u.cycle(apply=True)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(2)[7:19],
                         "on applique le digest évalué, pas celui qui vient de bouger")


# ── Veille de sécurité ──────────────────────────────────────────────────────

def advisory(clock, ghsa="GHSA-2222-3333-4444", sev="high", hours_ago=1.0, rng="< 3.6.3",
             cve="CVE-2026-12345", summary="Contournement d'authentification", withdrawn=False):
    import datetime as _dt
    when = _dt.datetime.fromtimestamp(clock.t - hours_ago * 3600, _dt.timezone.utc).isoformat()
    return {"ghsa_id": ghsa, "cve_id": cve, "severity": sev, "published_at": when, "summary": summary,
            "withdrawn_at": "2026-01-01T00:00:00Z" if withdrawn else None,
            "vulnerabilities": [{"package": {"ecosystem": "go", "name": "x"}, "vulnerable_version_range": rng,
                                 "patched_versions": "3.6.3"}]}


class TestVersions(unittest.TestCase):
    def test_parse_version(self):
        self.assertEqual(du.parse_version("3.6.3"), (3, 6, 3, 1))
        self.assertEqual(du.parse_version("v3.6.3"), (3, 6, 3, 1))
        self.assertEqual(du.parse_version("3.6.3-rc1"), (3, 6, 3, 0))
        self.assertEqual(du.parse_version("3.6.3+build5"), (3, 6, 3, 1))
        for bad in ("", None, "latest", "3.6", "3", "v3.x.1", "3.6.3.1", "1.0.0 beta"):
            self.assertIsNone(du.parse_version(bad), bad)

    def test_ranges(self):
        cases = [("3.6.2", "< 3.6.3", True), ("3.6.3", "< 3.6.3", False), ("3.6.4", "< 3.6.3", False),
                 ("3.0.0", ">= 3.0.0, < 3.6.3", True), ("2.11.9", ">= 3.0.0, < 3.6.3", False),
                 ("3.6.3", "<= 3.6.3", True), ("3.6.3", "> 3.6.3", False), ("3.6.3", "= 3.6.3", True),
                 ("3.6.3", "3.6.3", True), ("v3.6.2", "< 3.6.3", True), ("3.6.3-rc1", "< 3.6.3", True),
                 ("1.9.9", "< 1.10.0", True), ("1.10.0", "< 1.10", False), ("2.0.0", "< 10.0.0", True)]
        for v, r, want in cases:
            self.assertEqual(du.in_range(v, r), want, f"{v} dans {r}")

    def test_unreadable_never_guesses(self):
        for v, r in (("latest", "< 3.6.3"), ("3.6.2", ""), ("3.6.2", "autour de 3.6"), ("3.6.2", "<< 3"), ("3.6.2", ">= x.y")):
            with self.assertRaises(ValueError, msg=f"{v!r} {r!r}"):
                du.in_range(v, r)

    def test_clean_text(self):
        self.assertEqual(du.clean_text("a\n\n b\x00c\t d"), "a b c d")
        self.assertEqual(len(du.clean_text("x" * 999)), 200)
        self.assertEqual(du.clean_text(None), "")

    def test_compact_advisory(self):
        clock = Clock()
        a = du.compact_advisory(advisory(clock))
        self.assertEqual((a["id"], a["cve"], a["sev"], a["ranges"]), ("GHSA-2222-3333-4444", "CVE-2026-12345", "high", ["< 3.6.3"]))
        self.assertFalse(a["withdrawn"])
        self.assertTrue(du.compact_advisory(advisory(clock, withdrawn=True))["withdrawn"])
        self.assertEqual(du.compact_advisory(advisory(clock, cve="pas-un-cve"))["cve"], "")
        for bad in (None, 3, {}, {"ghsa_id": "GHSA-aaaa-bbbb-cccc"}, dict(advisory(clock), published_at="hier"),
                    dict(advisory(clock), ghsa_id="n'importe quoi")):
            self.assertIsNone(du.compact_advisory(bad), bad)
        weird = du.compact_advisory(dict(advisory(clock), summary="@everyone\n```rm -rf```\x00" + "z" * 500))
        self.assertNotIn("\n", weird["summary"])
        self.assertLessEqual(len(weird["summary"]), 200)

    def test_config_security_keys(self):
        one = {"name": "a", "compose_dir": "/x", "service": "a"}
        cfg = du.parse_config({"service": [dict(one, advisories=["traefik/traefik"], min_severity="Critical",
                                                security_cooldown="1h", security_policy="alert")]})
        s = cfg.services[0]
        self.assertEqual((s.advisories, s.min_severity, s.security_cooldown, s.security_policy),
                         (["traefik/traefik"], "critical", 3600, "alert"))
        self.assertEqual(cfg.services[0].security_policy, "alert")
        defaults = du.parse_config({"service": [one]}).services[0]
        self.assertEqual((defaults.advisories, defaults.min_severity, defaults.security_cooldown, defaults.security_policy),
                         ([], "high", 6 * 3600, "fast"))
        bad = [dict(one, advisories=["pas un depot"]), dict(one, advisories="traefik/traefik"),
               dict(one, advisories=["../../etc/passwd"]), dict(one, min_severity="énorme"),
               dict(one, security_policy="yolo"), dict(one, security_cooldown="vite")]
        for b in bad:
            with self.assertRaises(du.ConfigError, msg=str(b)):
                du.parse_config({"service": [b]})
        for st in ({"github_api": "ftp://x"}, {"github_api": "https://api.github.com/chemin"}, {"advisory_poll": "1m"}):
            with self.assertRaises(du.ConfigError, msg=str(st)):
                du.parse_config({"settings": st, "service": [one]})
        with self.assertRaises(du.ConfigError):  # un groupe doit avoir la même politique de sécurité
            du.parse_config({"service": [dict(one, group="g", advisories=["a/b"]), dict(one, name="b", group="g")]})


class TestSecurity(Base):
    def setUp(self):
        super().setUp()
        self.world.add("traefik", "/stack/traefik", "traefik", "traefik:v3.6", 1, version="3.6.2")
        self.new_remote(n=2)
        self.world.digest_versions[dg(2)] = "3.6.3"
        self.gh["traefik/traefik"] = (200, [advisory(self.clock)], "etag-1")

    def mk(self, **kw):
        svc = self.svc(advisories=["traefik/traefik"], **kw.pop("svc", {}))
        return self.make([svc], **kw)

    def posted(self) -> str:
        return "\n".join(p[1]["content"] for p in self.posts)

    def test_fast_track_waits_security_cooldown_then_applies(self):
        u = self.mk()
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("Correctif de sécurité détecté", self.posted())
        self.assertIn("GHSA-2222-3333-4444", self.posted())
        self.assertIn("CORRECTIF DE SÉCURITÉ", self.logtext())
        self.clock.days(5 / 24)                      # +5 h : le délai de sécurité (6 h) n'est pas écoulé
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.clock.days(2 / 24)                      # +7 h : 3 jours n'ont PAS passé, mais le correctif est confirmé
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-" + dg(2)[7:19])
        self.assertIn("Correctif de sécurité appliqué : GHSA-2222-3333-4444", self.posted())
        self.assertEqual(self.posted().count("Correctif de sécurité détecté"), 1, "alerte non répétée")

    def test_clock_starts_at_the_latest_of_first_seen_and_publication(self):
        # avis publié il y a 2 j, image vue pour la première fois maintenant : on attend 6 h APRÈS la première observation
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, hours_ago=48)], "e")
        u = self.mk()
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1)

    def test_a_brand_new_advisory_is_not_instant(self):
        # image vue depuis 3 j (cooldown normal écoulé) mais avis publié il y a 1 h : le raccourci ne s'applique pas
        # (et le délai normal étant écoulé, la mise à jour a de toute façon lieu — le raccourci ne retarde jamais)
        u = self.mk(svc={"cooldown": "0"})
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1, "un raccourci ne peut que raccourcir")

    def test_candidate_still_vulnerable_keeps_normal_delay(self):
        self.world.digest_versions[dg(2)] = "3.6.2"        # la « nouvelle » image porte encore la version touchée
        u = self.mk()
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("pas de correctif dans la nouvelle image", self.posted())
        self.clock.days(3)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1, "le délai normal reste appliqué")

    def test_candidate_version_unreadable_is_alert_only(self):
        self.world.digest_versions.pop(dg(2))
        u = self.mk()
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("pas lisible", self.posted())
        self.assertIn("docker-update now traefik --yes", self.posted())

    def test_alert_only_policy(self):
        u = self.mk(svc={"security_policy": "alert"})
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [], "politique alert : jamais d'accélération")
        self.assertIn("docker-update now traefik --yes", self.posted())

    def test_severity_and_lookback_filters(self):
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, sev="medium"),
                                            advisory(self.clock, ghsa="GHSA-5555-6666-7777", hours_ago=24 * 40)], "e")
        u = self.mk()
        u.cycle(apply=True)
        self.assertEqual(self.cmds(["docker", "pull"]), [], "aucun avis pertinent : aucun téléchargement anticipé")
        self.assertEqual(self.posts, [])
        u2 = self.mk(svc={"min_severity": "medium"})
        u2.cycle(apply=True)
        self.assertIn("Correctif de sécurité détecté", self.posted())

    def test_withdrawn_advisory_is_ignored(self):
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, withdrawn=True)], "e")
        self.mk().cycle(apply=True)
        self.assertEqual(self.posts, [])

    def test_unaffected_current_version_is_ignored(self):
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, rng=">= 2.0.0, < 3.0.0")], "e")
        self.mk().cycle(apply=True)
        self.assertEqual(self.posts, [])
        self.assertEqual(self.cmds(["docker", "pull"]), [])

    def test_critical_ignores_window_but_high_does_not(self):
        for sev, expected in (("high", 0), ("critical", 1)):
            with self.subTest(sev):
                self.setUp()
                self.minutes = 14 * 60          # 14 h : hors de la fenêtre 02:00-05:00 (après setUp, qui la remet à 03:00)
                self.gh["traefik/traefik"] = (200, [advisory(self.clock, sev=sev)], "e")
                u = self.mk(svc={"window": "02:00-05:00"})
                u.cycle(apply=True)
                self.clock.days(7 / 24)
                u.cycle(apply=True)
                self.assertEqual(len(self.world.ups), expected)
        self.assertIn("CRITIQUE", self.logtext())

    def test_up_to_date_but_vulnerable_alerts_once(self):
        self.world.registry["traefik:v3.6"] = manifest(1)       # l'étiquette n'a pas bougé
        u = self.mk()
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.clock.days(1)
        u.cycle(apply=True)
        self.assertEqual(self.posted().count("VULNÉRABLE"), 1)
        self.assertIn("3.6.2", self.posted())
        self.assertEqual(self.world.ups, [])

    def test_unknown_current_version_alerts_once(self):
        self.world.labels.clear()
        u = self.mk()
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.assertEqual(self.posted().count("version en cours"), 1)
        self.assertEqual(self.world.ups, [])

    def test_unreadable_range_is_alert_only(self):
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, rng="autour de la 3.6")], "e")
        u = self.mk()
        u.cycle(apply=True)
        self.assertIn("plage de versions illisible", self.posted())
        self.assertEqual(self.cmds(["docker", "pull"]), [])

    def test_check_never_pulls_or_notifies(self):
        u = self.mk()
        u.cycle(apply=False)
        self.assertEqual(self.cmds(["docker", "pull"]), [])
        self.assertEqual(self.posts, [])
        self.assertIn("prochain passage", self.logtext())
        self.assertEqual(u.state.data["alerts"], {})

    def test_dry_run_neither_pulls_nor_persists(self):
        u = self.mk(dry_run=True, persist=False)
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(self.cmds(["docker", "pull"]), [])
        self.assertEqual(self.world.ups, [])
        self.assertFalse(os.path.exists(self.state_path))
        self.assertIn("simulation", self.logtext())
        self.assertNotIn("pas lisible", self.posted(), "pas de fausse alerte en simulation")

    def test_failed_fast_track_is_not_retried(self):
        self.world.fail_up.add("id-" + dg(2)[7:19])
        u = self.mk()
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        self.assertEqual(u.cycle(apply=True), 1)
        n = len(self.world.ups)
        self.clock.days(1)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), n)
        self.assertEqual(self.world.containers[("/stack/traefik", "traefik")]["image_id"], "id-traefik-old")

    def test_github_outage_does_not_block_normal_updates(self):
        self.gh["traefik/traefik"] = (500, None, "")
        u = self.mk(svc={"cooldown": "1d"})
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.assertEqual(self.logtext().count("injoignable"), 1, "l'indisponibilité n'est journalisée qu'une fois")
        self.clock.days(1.1)
        self.assertEqual(u.cycle(apply=True), 0)
        self.assertEqual(len(self.world.ups), 1)
        self.assertEqual(self.posts and "Correctif de sécurité détecté" in self.posted(), False)

    def test_rate_limit_returns_old_items(self):
        u = self.mk()
        u.cycle(apply=True)
        self.clock.days(2 / 24)
        self.gh["traefik/traefik"] = (403, None, "")             # limite de débit
        u.cycle(apply=True)
        self.clock.days(5 / 24)
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1, "les anciens avis restent connus malgré l'erreur")

    def test_poll_interval_and_etag(self):
        u = self.mk()
        u.cycle(apply=True)
        u.cycle(apply=True)
        self.assertEqual(len(self.gh_calls), 1, "pas de nouvelle requête dans l'intervalle")
        self.clock.days(2 / 24)
        u.cycle(apply=True)
        self.assertEqual(len(self.gh_calls), 2)
        self.assertEqual(self.gh_calls[1][2], "etag-1", "requête conditionnelle")
        self.assertEqual(u.state.data["advisories"]["traefik/traefik"]["items"][0]["id"], "GHSA-2222-3333-4444")

    def test_token_is_sent_but_never_logged(self):
        tf = os.path.join(self.tmp.name, "gh_token")
        with open(tf, "w") as f:
            f.write("jeton-SUPER-SECRET\n")
        u = self.mk(settings={"github_token_file": tf})
        u.cycle(apply=True)
        self.assertEqual(self.gh_calls[0][1], "jeton-SUPER-SECRET")
        self.assertNotIn("SUPER-SECRET", self.logtext() + self.posted() + json.dumps(u.state.data))

    def test_untrusted_summary_cannot_inject(self):
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, summary="@everyone\n**URGENT** ```rm -rf /``` " + "z" * 900)], "e")
        self.world.labels.clear()                                  # force l'alerte qui cite le résumé
        self.mk().cycle(apply=True)
        content = self.posts[0][1]["content"]
        self.assertEqual(self.posts[0][1]["allowed_mentions"], {"parse": []})
        self.assertLessEqual(len(content), du.DISCORD_MAX)
        self.assertEqual(content.count("\n"), 1, "titre + une seule ligne de texte")

    def test_old_alerts_are_pruned(self):
        u = self.mk()
        u.state.data["alerts"] = {"vieille": int(self.clock.t) - 130 * DAY, "recente": int(self.clock.t) - 5 * DAY}
        u.cycle(apply=True)
        self.assertEqual(sorted(u.state.data["alerts"]).count("vieille"), 0)
        self.assertIn("recente", u.state.data["alerts"])

    def test_publication_date_delays_a_late_advisory(self):
        # l'image est vue à t0 ; l'avis n'est publié qu'à t0+10 h. Le délai de sécurité (6 h) court depuis la
        # plus tardive des deux dates : prêt à t0+16 h, pas à t0+6 h.
        self.gh["traefik/traefik"] = (200, [], "e0")
        u = self.mk()
        u.cycle(apply=True)                                     # t0 : rien à signaler
        self.clock.days(10 / 24)
        self.gh["traefik/traefik"] = (200, [advisory(self.clock, hours_ago=0)], "e1")
        u.cycle(apply=True)                                     # t0+10 h : avis tout frais
        self.assertEqual(self.world.ups, [])
        self.clock.days(2 / 24)                                 # t0+12 h : > 6 h depuis la 1re observation
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [], "il faut attendre 6 h APRÈS la publication de l'avis")
        self.clock.days(5 / 24)                                 # t0+17 h
        u.cycle(apply=True)
        self.assertEqual(len(self.world.ups), 1)

    def test_unreadable_second_range_blocks_acceleration(self):
        a = advisory(self.clock)
        a["vulnerabilities"].append({"vulnerable_version_range": "plage bizarre"})
        self.gh["traefik/traefik"] = (200, [a], "e")
        u = self.mk()
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [], "on ne conclut jamais « corrigé » sur une plage qu'on ne sait pas lire")
        self.assertIn("pas de correctif dans la nouvelle image", self.posted())

    def test_check_reports_detected_version_or_its_absence(self):
        u = self.mk()
        u.cycle(apply=False)
        self.assertIn("version détectée 3.6.2", self.logtext())
        self.logs.clear()
        self.world.labels.clear()
        u.cycle(apply=False)
        self.assertIn("version NON détectée", self.logtext())

    def test_token_not_logged_when_the_api_fails(self):
        tf = os.path.join(self.tmp.name, "gh_token")
        with open(tf, "w") as f:
            f.write("jeton-SUPER-SECRET\n")
        self.gh["traefik/traefik"] = (500, None, "")
        u = self.mk(settings={"github_token_file": tf})
        u.cycle(apply=True)
        self.assertIn("injoignable", self.logtext())
        self.assertNotIn("SUPER-SECRET", self.logtext() + self.posted() + json.dumps(u.state.data))

    def test_notify_mode_service_gets_an_alert_not_an_acceleration(self):
        u = self.mk(svc={"mode": "notify"})
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual(self.world.ups, [])
        self.assertIn("mode non automatique", self.posted())
        self.assertIn("docker-update now traefik --yes", self.posted())
        self.assertNotIn("Mise à jour accélérée", self.posted())

    def test_group_gets_one_accelerated_update(self):
        for sname in ("migrate", "web"):
            self.world.add(sname, "/stack/t", sname, "ghcr.io/org/tirelire:1.2", 1, version="1.2.0")
        self.world.registry["ghcr.io/org/tirelire:1.2"] = manifest(7)
        self.world.digest_versions[dg(7)] = "1.2.1"
        self.gh["org/tirelire"] = (200, [advisory(self.clock, rng="< 1.2.1")], "e")
        stack = [self.svc(sname, "/stack/t", sname, group="t", advisories=["org/tirelire"]) for sname in ("migrate", "web")]
        u = self.make(stack)
        u.cycle(apply=True)
        self.clock.days(7 / 24)
        u.cycle(apply=True)
        self.assertEqual([s for s, _ in self.world.ups], ["migrate", "web"])
        self.assertEqual(self.posted().count("Mise à jour réussie"), 1)


class TestGithubGet(unittest.TestCase):
    """Le vrai client urllib contre un petit serveur local."""

    def setUp(self):
        import http.server
        import threading
        self.seen: list[dict] = []
        outer = self

        class H(http.server.BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                outer.seen.append({"path": self.path, **{k.lower(): v for k, v in self.headers.items()}})
                if self.path.startswith("/repos/ok/"):
                    if self.headers.get("If-None-Match") == '"v1"':
                        self.send_response(304)
                        self.send_header("ETag", '"v1"')
                        self.end_headers()
                        return
                    body = json.dumps([{"ghsa_id": "GHSA-2222-3333-4444"}]).encode()
                    self.send_response(200)
                    self.send_header("ETag", '"v1"')
                elif self.path.startswith("/repos/broken/"):
                    body = b"{pas du json"
                    self.send_response(200)
                elif self.path.startswith("/repos/limit/"):
                    body = b"{}"
                    self.send_response(403)
                else:
                    body = b"{}"
                    self.send_response(404)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.addCleanup(self.srv.server_close)
        self.addCleanup(self.srv.shutdown)
        self.base = f"http://127.0.0.1:{self.srv.server_address[1]}"

    def test_ok_headers_and_etag(self):
        code, body, etag = du.github_get(self.base + "/repos/ok/x/security-advisories")
        self.assertEqual((code, etag), (200, '"v1"'))
        self.assertEqual(body[0]["ghsa_id"], "GHSA-2222-3333-4444")
        h = self.seen[0]
        self.assertTrue(h["user-agent"].startswith("docker-update"))
        self.assertIn("github", h["accept"])
        self.assertEqual(h["x-github-api-version"], "2022-11-28")
        self.assertNotIn("authorization", h, "pas de jeton configuré : aucun en-tête d'autorisation")

    def test_token_and_conditional_request(self):
        code, body, etag = du.github_get(self.base + "/repos/ok/x/security-advisories", "tok123", '"v1"')
        self.assertEqual((code, body), (304, None))
        self.assertEqual(self.seen[0]["authorization"], "Bearer tok123")
        self.assertEqual(self.seen[0]["if-none-match"], '"v1"')

    def test_errors(self):
        self.assertEqual(du.github_get(self.base + "/repos/missing/x/security-advisories")[:2], (404, None))
        self.assertEqual(du.github_get(self.base + "/repos/limit/x/security-advisories")[:2], (403, None))
        self.assertEqual(du.github_get(self.base + "/repos/broken/x/security-advisories")[0], 0, "JSON invalide -> 0")
        self.srv.shutdown()
        self.assertEqual(du.github_get(self.base + "/repos/ok/x/security-advisories")[0], 0, "connexion refusée -> 0")


class TestMain(Base):
    def write_cfg(self, extra=""):
        p = os.path.join(self.tmp.name, "updates.toml")
        with open(p, "w") as f:
            f.write(f'''[settings]
state_file = "{self.state_path}"
lock_file = "{self.tmp.name}/lock"
[[service]]
name = "a"
compose_dir = "{self.tmp.name}"
service = "a"
{extra}
''')
        return p

    def test_validate_and_status_and_hold(self):
        p = self.write_cfg()
        with mock.patch("sys.stdout", new=io.StringIO()) as out:
            self.assertEqual(du.main(["--config", p, "--allow-untrusted-config", "hold", "a"]), 0)
            self.assertEqual(du.main(["--config", p, "--allow-untrusted-config", "status"]), 0)
        self.assertIn("en pause : a", out.getvalue())

    def test_notify_test_command(self):
        p = self.write_cfg()
        with mock.patch("sys.stdout", new=io.StringIO()) as out:
            self.assertEqual(du.main(["--config", p, "--allow-untrusted-config", "notify-test"]), 1)
        self.assertIn("aucune URL", out.getvalue())

    def test_bad_config_exit_code_2(self):
        p = self.write_cfg('mode = "turbo"')
        with mock.patch("sys.stdout", new=io.StringIO()):
            self.assertEqual(du.main(["--config", p, "--allow-untrusted-config", "validate"]), 2)

    def test_lock_prevents_concurrent_runs(self):
        import fcntl
        p = self.write_cfg()
        held = open(os.path.join(self.tmp.name, "lock"), "w")
        fcntl.flock(held, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.addCleanup(held.close)
        with mock.patch("sys.stdout", new=io.StringIO()) as out:
            self.assertEqual(du.main(["--config", p, "--allow-untrusted-config", "run"]), 0)
        self.assertIn("autre passage", out.getvalue())

    def test_untrusted_config_refused_as_root(self):
        p = self.write_cfg()
        fake = mock.Mock(st_uid=1000, st_mode=0o100644)
        with mock.patch("os.geteuid", return_value=0), mock.patch("os.stat", return_value=fake), \
                mock.patch("sys.stdout", new=io.StringIO()):
            self.assertEqual(du.main(["--config", p, "validate"]), 2)

    def test_heartbeat(self):
        self.traefik()
        hb = os.path.join(self.tmp.name, "hb")
        with open(hb, "w") as f:
            f.write("https://kuma.test/api/push/XYZ\n")
        u = self.make([self.svc()], settings={"heartbeat_url_file": hb})
        seen = []
        u.http = lambda url: (seen.append(url), 200)[1]
        u.heartbeat()
        self.assertEqual(seen, ["https://kuma.test/api/push/XYZ"])
        u.http = lambda url: 500
        u.heartbeat()
        self.assertIn("signal de vie non reçu", self.logtext())
        self.assertNotIn("XYZ", self.logtext())
        seen.clear()
        u.run.dry_run = True
        u.http = lambda url: (seen.append(url), 200)[1]
        u.heartbeat()
        self.assertEqual(seen, [], "pas de signal en simulation")


if __name__ == "__main__":
    unittest.main(verbosity=2)
