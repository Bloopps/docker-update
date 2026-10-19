#!/usr/bin/env python3
"""docker-update : mises à jour d'images pour des piles Docker Compose, avec délai de sécurité.

Principe
  - Aucun conteneur ne détient le droit de créer d'autres conteneurs : ce script tourne sur l'hôte.
  - Une image n'est appliquée que si son digest est resté le même pendant `cooldown` (mesuré depuis
    la première fois que CE script l'a vu, pas depuis une date écrite dans l'image).
  - On tire exactement le digest évalué (jamais « le tag »), puis on recrée le service avec compose.
  - Si le service ne repart pas : alerte, aucun retour arrière automatique (l'image précédente est conservée).

Fonctions
  - un conteneur arrêté n'est jamais touché (ni mise à jour, ni veille, ni nettoyage) ;
  - modes par service : auto (applique après le délai), notify (prévient seulement), manual (ignore) ;
  - groupes (plusieurs services d'une même image, mis à jour ensemble dans l'ordre du fichier) ;
  - fenêtre horaire (ex. 02:00-05:00) : on observe tout le temps, on n'applique que dans la fenêtre ;
  - test de fumée HTTP après la mise à jour ; nettoyage de l'image précédente une fois la mise à jour réussie ;
  - pause (hold) ;
  - notifications Discord (ntfy ou JSON possibles), anti-spam des erreurs répétées ;
  - signal de vie (heartbeat) pour détecter un arrêt silencieux de l'outil ;
  - veille de sécurité : lit les avis de sécurité publiés par les dépôts GitHub des projets suivis ; si la
    version en cours est affectée ET que l'image candidate (version lue dans ses étiquettes) ne l'est plus,
    le délai est raccourci (`security_cooldown`, 6 h par défaut). Tout cas ambigu devient une ALERTE seule.

Sous-commandes
  validate          vérifie la configuration et les outils, ne touche à rien
  check             observe (registre + état + tests de fumée), n'applique RIEN, n'envoie rien
  run               observe, applique les mises à jour « auto » dont le délai est écoulé, notifie
  now NOM... --yes  applique tout de suite (sans délai ni fenêtre) les services ou groupes nommés
  hold NOM...       met en pause (aucune mise à jour automatique) ; unhold NOM... reprend
  status            affiche l'état mémorisé
  notify-test       envoie un message de test (vérifie le webhook), ne touche à rien

Bibliothèque standard seulement (Python >= 3.11).
"""
from __future__ import annotations

import argparse
import datetime
import fcntl
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
import tomllib
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Callable

NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
MODES = ("auto", "notify", "manual")
REGISTRY_TOOLS = ("buildx", "regctl")
NOTIFY_FORMATS = ("discord", "ntfy", "json")
UNITS = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}
DISCORD_MAX = 1900
MAX_MANIFEST = 4 << 20
MAX_API_BODY = 2 << 20
MAX_RELEASES_BODY = 8 << 20
RELEASES_FIRST, RELEASES_NEXT = 30, 10      # releases demandées à la 1re synchronisation, puis aux passages suivants
RELEASES_RESYNC = 7 * 86400                 # sans synchronisation complète depuis ce délai : on relit les 30
SEVERITIES = ("low", "medium", "high", "critical")
REPO_RE = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
GHSA_RE = re.compile(r"^GHSA(-[23456789cfghjmpqrvwx]{4}){3}$")
POLICIES = ("fast", "alert")
RUNNING_STATES = ("running", "restarting")   # les autres états (exited, created, paused, dead…) : conteneur laissé tel quel
FLOATING_TAGS = {"latest", "main", "master", "dev", "edge", "nightly", "stable"}


class ConfigError(Exception):
    pass


class UpdateError(Exception):
    pass


# ── Durées, références d'image ──────────────────────────────────────────────

def parse_duration(value) -> int:
    s = str(value).strip()
    if s == "0":
        return 0
    m = re.fullmatch(r"(\d+)([smhdw])", s)
    if not m:
        raise ConfigError(f"durée invalide : {value!r} (exemples : 30m, 6h, 3d, 1w, 0)")
    return int(m[1]) * UNITS[m[2]]


def fmt_duration(sec: int) -> str:
    for unit, n in (("j", 86400), ("h", 3600), ("min", 60)):
        if sec >= n:
            return f"{sec // n} {unit}"
    return f"{sec} s"


def parse_window(spec) -> tuple[int, int] | None:
    """« 02:00-05:00 » -> (120, 300) en minutes depuis minuit ; vide -> None (toujours)."""
    s = str(spec).strip()
    if not s:
        return None
    m = re.fullmatch(r"(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})", s)
    if not m:
        raise ConfigError(f"fenêtre invalide : {spec!r} (exemple : 02:00-05:00)")
    h1, m1, h2, m2 = (int(x) for x in m.groups())
    if h1 > 23 or h2 > 23 or m1 > 59 or m2 > 59:
        raise ConfigError(f"fenêtre invalide : {spec!r}")
    a, b = h1 * 60 + m1, h2 * 60 + m2
    if a == b:
        raise ConfigError(f"fenêtre vide : {spec!r}")
    return a, b


def in_window(w: tuple[int, int] | None, minutes: int) -> bool:
    if w is None:
        return True
    a, b = w
    return a <= minutes < b if a < b else (minutes >= a or minutes < b)


def short_id(image_id: str) -> str:
    """« sha256:0123456789ab… » -> « 0123456789ab » (comme docker) ; autre forme : telle quelle, bornée."""
    s = str(image_id or "")
    return s[7:19] if s.startswith("sha256:") else clean_text(s, 40)


def clean_text(value, limit: int = 200) -> str:
    """Texte venu de l'extérieur : sans caractères de contrôle, sur une ligne, borné."""
    s = "".join(c if c.isprintable() else " " for c in str(value or ""))
    return " ".join(s.split())[:limit]


_VER = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)(?:([-+])([0-9A-Za-z.-]+))?$")
_BOUND = re.compile(r"^(>=|<=|>|<|=)?\s*v?(\d+)\.(\d+)(?:\.(\d+))?(?:([-+])([0-9A-Za-z.-]+))?$")


def parse_version(value) -> tuple[int, int, int, int] | None:
    """« v3.6.3 » -> (3, 6, 3, 1). Le dernier élément vaut 0 pour une préversion (donc plus petite)."""
    m = _VER.match(str(value or "").strip())
    if not m:
        return None
    return int(m[1]), int(m[2]), int(m[3]), 0 if m[4] == "-" else 1


def in_range(version, rng: str) -> bool:
    """La version est-elle dans une plage d'avis GitHub (« >= 3.0.0, < 3.6.3 », contraintes liées par ET) ?
    Lève ValueError si la version ou la plage sont illisibles : on ne devine jamais."""
    v = parse_version(version)
    if v is None:
        raise ValueError(f"version illisible : {version!r}")
    parts = [p.strip() for p in str(rng).split(",") if p.strip()]
    if not parts:
        raise ValueError("plage vide")
    for p in parts:
        m = _BOUND.match(p)
        if not m:
            raise ValueError(f"contrainte illisible : {p!r}")
        bound = (int(m[2]), int(m[3]), int(m[4] or 0), 0 if m[5] == "-" else 1)
        op = m[1] or "="
        ok = {">=": v >= bound, ">": v > bound, "<=": v <= bound, "<": v < bound, "=": v == bound}[op]
        if not ok:
            return False
    return True


def sev_rank(s: str) -> int:
    s = (s or "").lower()
    return SEVERITIES.index(s) if s in SEVERITIES else 1  # sévérité inconnue : traitée comme « moyenne »


def compact_advisory(a) -> dict | None:
    """Réduit un avis GitHub aux champs utiles ; None si inutilisable."""
    if not isinstance(a, dict):
        return None
    ghsa = str(a.get("ghsa_id") or "")
    if not GHSA_RE.match(ghsa):
        return None
    try:
        pub = int(datetime.datetime.fromisoformat(str(a.get("published_at")).replace("Z", "+00:00")).timestamp())
    except ValueError:
        return None
    ranges = []
    for v in a.get("vulnerabilities") or []:
        if isinstance(v, dict) and isinstance(v.get("vulnerable_version_range"), str) and v["vulnerable_version_range"].strip():
            ranges.append(v["vulnerable_version_range"].strip())
    cve = str(a.get("cve_id") or "")
    return {"id": ghsa, "cve": cve if re.match(r"^CVE-\d{4}-\d{4,}$", cve) else "",
            "sev": str(a.get("severity") or "").lower(), "pub": pub, "ranges": ranges,
            "summary": clean_text(a.get("summary")), "withdrawn": bool(a.get("withdrawn_at"))}


_RVER = re.compile(r"^v?(\d{1,6})\.(\d{1,6})(?:\.(\d{1,6}))?(?:([-+])([0-9A-Za-z.-]{1,64}))?$")


def parse_release_version(value) -> tuple[int, int, int, int] | None:
    """Version d'une étiquette de release ou d'une image : « v2.45.1 », « 24.04 », « 2026.7.8 ».
    Plus souple que parse_version (X.Y accepté, patch = 0), mais toujours stricte : « latest », « main » ou
    « 18-vectorchord0.5.3 » donnent None. Dernier élément : 0 pour une pré-version (donc plus petite)."""
    m = _RVER.match(str(value or "").strip())
    if not m:
        return None
    return int(m[1]), int(m[2]), int(m[3] or 0), 0 if m[4] == "-" else 1


# Motifs « forts » des notes de release : mots entiers, quantificateurs bornés (pas de retour arrière coûteux).
_RN = re.IGNORECASE
RELEASE_PATTERNS: tuple[tuple[str, re.Pattern], ...] = (
    ("CVE", re.compile(r"\bCVE-\d{4}-\d{4,}\b", _RN)),
    ("GHSA", re.compile(r"\bGHSA(?:-[23456789cfghjmpqrvwx]{4}){3}\b", _RN)),
    ("correctif de sécurité", re.compile(
        r"\bsecurity[ \t]+(?:fix(?:es)?|updates?|releases?|patch(?:es)?|advisor(?:y|ies))\b", _RN)),
    ("vulnérabilité", re.compile(r"\bvulnerab\w{0,20}", _RN)),
    ("RCE/XSS/SSRF/CSRF", re.compile(r"\b(?:RCE|XSS|SSRF|CSRF)\b", _RN)),
    ("injection SQL", re.compile(r"\bSQL[ \t]+injections?\b", _RN)),
    ("élévation de privilèges", re.compile(r"\bprivilege[ \t]+escalations?\b", _RN)),
    ("contournement d'authentification", re.compile(r"\bauth\w{0,15}[ \t]+bypass(?:es)?\b", _RN)),
    ("path traversal", re.compile(r"\bpath[ \t]+traversals?\b", _RN)),
)
# Le mot « security » seul est trop bruyant : signal faible (alerte d'information, regroupée).
RELEASE_WEAK = re.compile(r"\bsecurity\b", _RN)
TAG_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+-]{0,79}$")
MAX_NOTES = 64 << 10          # on n'analyse que les 64 premiers Kio d'une note de release
MAX_STRONG_HITS, MAX_WEAK_HITS = 30, 3


def scan_notes(text) -> list[dict]:
    """Motifs de sécurité trouvés dans une note de release (texte non fiable : borné, jamais interprété).
    Chaque résultat : {k: famille, t: texte trouvé, x: extrait nettoyé, w: True si signal faible}."""
    body = text[:MAX_NOTES] if isinstance(text, str) else ""
    hits: list[dict] = []
    seen: set[tuple[str, str]] = set()
    for kind, pat in RELEASE_PATTERNS:
        if len(hits) >= MAX_STRONG_HITS:
            break
        for m in pat.finditer(body):
            key = (kind, m[0].lower())
            if key in seen:
                continue
            seen.add(key)
            hits.append({"k": kind, "t": clean_text(m[0], 60), "w": False,
                         "x": clean_text(body[max(0, m.start() - 70):m.end() + 70], 160)})
            if len(hits) >= MAX_STRONG_HITS:
                break
    weak = 0
    for m in RELEASE_WEAK.finditer(body):
        hits.append({"k": "security", "t": clean_text(m[0], 60), "w": True,
                     "x": clean_text(body[max(0, m.start() - 70):m.end() + 70], 160)})
        weak += 1
        if weak >= MAX_WEAK_HITS:
            break
    return hits


def compact_release(r) -> dict | None:
    """Réduit une release GitHub aux champs utiles (la note elle-même n'est pas conservée) ; None si inutilisable."""
    if not isinstance(r, dict):
        return None
    tag = r.get("tag_name")
    if not isinstance(tag, str) or not TAG_RE.match(tag):
        return None
    try:
        pub = int(datetime.datetime.fromisoformat(
            str(r.get("published_at") or r.get("created_at")).replace("Z", "+00:00")).timestamp())
    except ValueError:
        pub = 0
    return {"tag": tag, "pub": pub, "draft": bool(r.get("draft")), "pre": bool(r.get("prerelease")),
            "hits": scan_notes(r.get("body"))}


def split_ref(ref: str) -> tuple[str, str, str | None]:
    """(dépôt, étiquette, digest épinglé ou None). 'traefik:v3.6' -> ('traefik', 'v3.6', None)."""
    digest = None
    if "@" in ref:
        ref, digest = ref.split("@", 1)
    last = ref.rsplit("/", 1)[-1]
    if ":" in last:
        repo, tag = ref.rsplit(":", 1)
    else:
        repo, tag = ref, "latest"
    return repo, tag, digest


# ── Configuration ───────────────────────────────────────────────────────────

@dataclass
class Settings:
    state_file: str = "/var/lib/docker-update/state.json"
    lock_file: str = "/run/docker-update.lock"
    registry_tool: str = "buildx"
    notify_url_file: str = ""
    notify_format: str = "discord"
    default_cooldown: int = 3 * 86400
    settle_seconds: int = 15
    health_timeout: int = 180
    window: str = ""
    heartbeat_url_file: str = ""
    github_api: str = "https://api.github.com"
    github_token_file: str = ""
    advisory_poll: int = 3600
    advisory_lookback: int = 30 * 86400
    version_label: str = "org.opencontainers.image.version"
    cleanup: bool = True


@dataclass
class Service:
    name: str
    compose_dir: str
    service: str
    compose_files: list[str]
    mode: str
    cooldown: int
    group: str
    health_timeout: int
    window: tuple[int, int] | None = None
    smoke_url: str = ""
    smoke_expect: list[int] = field(default_factory=list)
    advisories: list[str] = field(default_factory=list)
    min_severity: str = "high"
    security_cooldown: int = 6 * 3600
    security_policy: str = "fast"
    cleanup: bool = True
    release_notes: bool = False
    release_notes_repos: list[str] = field(default_factory=list)
    release_notes_ignore: list[str] = field(default_factory=list)

    @property
    def unit(self) -> str:
        return self.group or self.name


@dataclass
class Config:
    settings: Settings
    services: list[Service]


_SETTINGS_KEYS = {"state_file", "lock_file", "registry_tool", "notify_url_file", "notify_format",
                  "default_cooldown", "settle_seconds", "health_timeout", "window", "heartbeat_url_file",
                  "github_api", "github_token_file", "advisory_poll", "advisory_lookback", "version_label", "cleanup"}
_SERVICE_KEYS = {"name", "compose_dir", "service", "compose_files", "mode", "cooldown", "group",
                 "health_timeout", "window", "smoke_url", "smoke_expect",
                 "advisories", "min_severity", "security_cooldown", "security_policy", "cleanup",
                 "release_notes", "release_notes_repos", "release_notes_ignore"}


_REMOVED_KEYS = {
    "rollback": "en cas d'échec l'outil alerte et ne touche à rien : aucun retour arrière automatique",
    "pre_hook": "la sauvegarde se gère en dehors de l'outil",
}


def _check_keys(where: str, data: dict, allowed: set[str]) -> None:
    for key, why in _REMOVED_KEYS.items():
        if key in data:
            raise ConfigError(f"{where} : « {key} » n'existe plus ({why})")
    unknown = set(data) - allowed
    if unknown:
        raise ConfigError(f"{where} : clé(s) inconnue(s) {sorted(unknown)} (faute de frappe ?)")


def parse_config(data: dict) -> Config:
    raw = dict(data.get("settings", {}))
    _check_keys("[settings]", raw, _SETTINGS_KEYS)
    st = Settings()
    for k in ("state_file", "lock_file", "notify_url_file", "heartbeat_url_file", "window",
              "github_api", "github_token_file", "version_label"):
        if k in raw:
            setattr(st, k, str(raw[k]))
    if "registry_tool" in raw:
        st.registry_tool = str(raw["registry_tool"])
    if st.registry_tool not in REGISTRY_TOOLS:
        raise ConfigError(f"registry_tool doit valoir {REGISTRY_TOOLS}")
    if "notify_format" in raw:
        st.notify_format = str(raw["notify_format"])
    if st.notify_format not in NOTIFY_FORMATS:
        raise ConfigError(f"notify_format doit valoir {NOTIFY_FORMATS}")
    parse_window(st.window)
    if not re.match(r"^https?://[^/\s]+$", st.github_api):
        raise ConfigError("github_api doit être une adresse http(s) sans chemin (ex. https://api.github.com)")
    if "advisory_poll" in raw:
        st.advisory_poll = parse_duration(raw["advisory_poll"])
        if st.advisory_poll < 300:
            raise ConfigError("advisory_poll : 5 minutes au minimum (la limite de GitHub est de 60 requêtes/h sans jeton)")
    if "advisory_lookback" in raw:
        st.advisory_lookback = parse_duration(raw["advisory_lookback"])
    if "default_cooldown" in raw:
        st.default_cooldown = parse_duration(raw["default_cooldown"])
    if "cleanup" in raw:
        if not isinstance(raw["cleanup"], bool):
            raise ConfigError("cleanup doit valoir true ou false")
        st.cleanup = raw["cleanup"]
    for k in ("settle_seconds", "health_timeout"):
        if k in raw:
            v = raw[k]
            if not isinstance(v, int) or isinstance(v, bool) or v < 0:
                raise ConfigError(f"{k} doit être un entier >= 0")
            setattr(st, k, v)

    services: list[Service] = []
    seen: set[str] = set()
    for i, s in enumerate(data.get("service", []), 1):
        where = f"[[service]] n°{i}"
        _check_keys(where, s, _SERVICE_KEYS)
        for req in ("name", "compose_dir", "service"):
            if req not in s:
                raise ConfigError(f"{where} : « {req} » est obligatoire")
        name = str(s["name"])
        if not NAME_RE.match(name):
            raise ConfigError(f"{where} : nom invalide {name!r}")
        if name in seen:
            raise ConfigError(f"{where} : nom en double {name!r}")
        seen.add(name)
        cdir = str(s["compose_dir"])
        if not os.path.isabs(cdir):
            raise ConfigError(f"{name} : compose_dir doit être un chemin absolu")
        svc_name = str(s["service"])
        if not NAME_RE.match(svc_name):
            raise ConfigError(f"{name} : nom de service compose invalide {svc_name!r}")
        files = s.get("compose_files", [])
        if not isinstance(files, list) or not all(isinstance(f, str) for f in files):
            raise ConfigError(f"{name} : compose_files doit être une liste de chaînes")
        files = [f if os.path.isabs(f) else os.path.join(cdir, f) for f in files]
        mode = str(s.get("mode", "notify"))
        if mode not in MODES:
            raise ConfigError(f"{name} : mode doit valoir {MODES}")
        group = str(s.get("group", ""))
        if group and not NAME_RE.match(group):
            raise ConfigError(f"{name} : nom de groupe invalide {group!r}")
        ht = s.get("health_timeout", st.health_timeout)
        if not isinstance(ht, int) or isinstance(ht, bool) or ht < 1:
            raise ConfigError(f"{name} : health_timeout doit être un entier >= 1")
        smoke_url = str(s.get("smoke_url", ""))
        smoke_expect = s.get("smoke_expect", [])
        if smoke_url:
            if not re.match(r"^https?://", smoke_url):
                raise ConfigError(f"{name} : smoke_url doit commencer par http:// ou https://")
            if not (isinstance(smoke_expect, list) and smoke_expect and all(
                    isinstance(c, int) and not isinstance(c, bool) and 100 <= c <= 599 for c in smoke_expect)):
                raise ConfigError(f"{name} : smoke_expect doit lister les codes HTTP acceptés (ex. [302, 401])")
        elif smoke_expect:
            raise ConfigError(f"{name} : smoke_expect sans smoke_url")
        advisories = s.get("advisories", [])
        if not isinstance(advisories, list) or not all(isinstance(r, str) and REPO_RE.match(r) for r in advisories):
            raise ConfigError(f"{name} : advisories doit être une liste de « propriétaire/dépôt » GitHub")
        min_sev = str(s.get("min_severity", "high")).lower()
        if min_sev not in SEVERITIES:
            raise ConfigError(f"{name} : min_severity doit valoir {SEVERITIES}")
        policy = str(s.get("security_policy", "fast"))
        if policy not in POLICIES:
            raise ConfigError(f"{name} : security_policy doit valoir {POLICIES}")
        cleanup = s.get("cleanup", st.cleanup)
        if not isinstance(cleanup, bool):
            raise ConfigError(f"{name} : cleanup doit valoir true ou false")
        rn = s.get("release_notes", False)
        if not isinstance(rn, bool):
            raise ConfigError(f"{name} : release_notes doit valoir true ou false")
        rn_repos = s.get("release_notes_repos", [])
        if not isinstance(rn_repos, list) or not all(isinstance(r, str) and REPO_RE.match(r) for r in rn_repos):
            raise ConfigError(f"{name} : release_notes_repos doit être une liste de « propriétaire/dépôt » GitHub")
        rn_ignore = s.get("release_notes_ignore", [])
        if not isinstance(rn_ignore, list) or not all(isinstance(p, str) and 0 < len(p) <= 200 for p in rn_ignore):
            raise ConfigError(f"{name} : release_notes_ignore doit être une liste d'expressions (200 caractères au plus)")
        for pat in rn_ignore:
            try:
                re.compile(pat, re.IGNORECASE)
            except re.error as e:
                raise ConfigError(f"{name} : release_notes_ignore : expression invalide {pat!r} ({e})")
        if not rn and (rn_repos or rn_ignore):
            raise ConfigError(f"{name} : release_notes_repos / release_notes_ignore sans release_notes = true")
        if rn and not (rn_repos or advisories):
            raise ConfigError(f"{name} : release_notes = true demande release_notes_repos (ou advisories) : "
                              f"le dépôt GitHub à lire")
        services.append(Service(
            name=name, compose_dir=cdir, service=svc_name, compose_files=files, mode=mode,
            cooldown=parse_duration(s["cooldown"]) if "cooldown" in s else st.default_cooldown,
            group=group, health_timeout=ht,
            window=parse_window(s["window"] if "window" in s else st.window),
            smoke_url=smoke_url, smoke_expect=list(smoke_expect), advisories=list(advisories),
            min_severity=min_sev, security_policy=policy, cleanup=cleanup, release_notes=rn,
            release_notes_repos=list(rn_repos), release_notes_ignore=list(rn_ignore),
            security_cooldown=parse_duration(s["security_cooldown"]) if "security_cooldown" in s else 6 * 3600))

    groups: dict[str, list[Service]] = {}
    for sv in services:
        if sv.group:
            groups.setdefault(sv.group, []).append(sv)
    for g, members in groups.items():
        if len({(m.mode, m.cooldown, m.window, tuple(m.advisories), m.min_severity,
                 m.security_cooldown, m.security_policy, m.cleanup) for m in members}) > 1:
            raise ConfigError(f"groupe {g} : mode, cooldown, window, cleanup et réglages de sécurité doivent être identiques")
        if g in seen:
            raise ConfigError(f"groupe {g} : même nom qu'un service")
    return Config(st, services)


def load_config(path: str) -> Config:
    try:
        with open(path, "rb") as f:
            data = tomllib.load(f)
    except FileNotFoundError:
        raise ConfigError(f"configuration introuvable : {path}")
    except tomllib.TOMLDecodeError as e:
        raise ConfigError(f"configuration illisible : {e}")
    return parse_config(data)


def check_trusted_file(path: str) -> None:
    """Quand on tourne en root, la configuration (qui désigne les piles compose pilotées en root) doit appartenir à root
    et ne pas être modifiable par un autre compte."""
    if os.geteuid() != 0:
        return
    st = os.stat(path)
    if st.st_uid != 0 or st.st_mode & 0o022:
        raise ConfigError(f"{path} doit appartenir à root et ne pas être modifiable par le groupe ou les autres "
                          f"(chown root:root, chmod 644 ou 600)")


# ── Exécution de commandes ──────────────────────────────────────────────────

@dataclass
class Result:
    rc: int
    out: str = ""
    err: str = ""


class Runner:
    """Lance des commandes sans shell. En simulation, les commandes « mutating » ne sont qu'affichées."""

    def __init__(self, dry_run: bool = False, log: Callable[[str], None] = print):
        self.dry_run = dry_run
        self.log = log

    def run(self, argv: list[str], mutating: bool = False, timeout: int = 900) -> Result:
        if mutating and self.dry_run:
            self.log("[simulation] " + shlex.join(argv))
            return Result(0)
        try:
            p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            return Result(124, "", f"délai dépassé ({timeout} s)")
        except FileNotFoundError:
            return Result(127, "", f"commande introuvable : {argv[0]}")
        return Result(p.returncode, p.stdout, p.stderr)

    def run_bytes(self, argv: list[str], timeout: int = 60) -> tuple[int, bytes, str]:
        """Comme run, mais la sortie reste en octets : indispensable pour hacher un manifeste."""
        try:
            p = subprocess.run(argv, capture_output=True, timeout=timeout)
        except subprocess.TimeoutExpired:
            return 124, b"", f"délai dépassé ({timeout} s)"
        except FileNotFoundError:
            return 127, b"", f"commande introuvable : {argv[0]}"
        return p.returncode, p.stdout, p.stderr.decode("utf-8", "replace")


# ── État mémorisé ───────────────────────────────────────────────────────────

class State:
    """images : ref -> {digest, first_seen, notified, failed} ; holds : noms en pause ;
    errors : dernière erreur connue par service (anti-spam). Écriture atomique."""

    def __init__(self, path: str):
        self.path = path
        self.data: dict = {"images": {}, "holds": [], "errors": {}, "advisories": {}, "alerts": {}, "releases": {}}
        try:
            with open(path, encoding="utf-8") as f:
                loaded = json.load(f)
        except FileNotFoundError:
            return
        except (OSError, json.JSONDecodeError):
            raise UpdateError(f"état illisible : {path} (le déplacer pour repartir de zéro)")
        if not isinstance(loaded, dict):
            raise UpdateError(f"état invalide : {path}")
        for k, default in (("images", {}), ("holds", []), ("errors", {}), ("advisories", {}), ("alerts", {}),
                           ("releases", {})):
            v = loaded.get(k, default)
            if not isinstance(v, type(default)):
                raise UpdateError(f"état invalide ({k}) : {path}")
            self.data[k] = v

    @property
    def images(self) -> dict:
        return self.data["images"]

    def get(self, ref: str) -> dict | None:
        return self.images.get(ref)

    def put(self, ref: str, rec: dict) -> None:
        self.images[ref] = rec

    def drop(self, ref: str) -> None:
        self.images.pop(ref, None)

    def save(self) -> None:
        d = os.path.dirname(self.path) or "."
        os.makedirs(d, mode=0o700, exist_ok=True)
        fd, tmp = tempfile.mkstemp(dir=d, prefix=".state-")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(self.data, f, indent=1, sort_keys=True)
            os.chmod(tmp, 0o600)
            os.replace(tmp, self.path)
        except BaseException:
            try:
                os.unlink(tmp)
            except OSError:
                pass
            raise


# ── Notifications ───────────────────────────────────────────────────────────

class Notifier:
    """Envoie un message court. L'URL (secret) est lue dans un fichier et n'est jamais journalisée."""

    def __init__(self, url_file: str, fmt: str, log: Callable[[str], None] = print,
                 post: Callable[[str, bytes, dict], None] | None = None, dry_run: bool = False):
        self.url_file, self.fmt, self.log, self.dry_run = url_file, fmt, log, dry_run
        self._post = post or self._http_post

    @staticmethod
    def _http_post(url: str, data: bytes, headers: dict) -> None:
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")
        with urllib.request.urlopen(req, timeout=10) as r:
            r.read()

    def _url(self) -> str:
        if not self.url_file:
            return ""
        try:
            with open(self.url_file, encoding="utf-8") as f:
                return f.read().strip()
        except OSError:
            return ""

    def send(self, title: str, text: str, level: str = "info") -> None:
        if self.dry_run:
            self.log(f"[simulation] notification : {title}")
            return
        url = self._url()
        if not url:
            return
        icon = {"info": "ℹ️", "ok": "✅", "warn": "⚠️", "error": "🚨"}.get(level, "")
        headers = {"User-Agent": "docker-update/1.0"}
        if self.fmt == "discord":
            content = f"{icon} **{title}**\n{text}"[:DISCORD_MAX]
            body = json.dumps({"content": content, "allowed_mentions": {"parse": []}}).encode()
            headers["Content-Type"] = "application/json"
        elif self.fmt == "ntfy":
            body = text.encode()
            headers["Title"] = title.encode("ascii", "replace").decode()
            headers["Priority"] = "high" if level == "error" else "default"
        else:
            body = json.dumps({"title": title, "text": text, "level": level}).encode()
            headers["Content-Type"] = "application/json"
        try:
            self._post(url, body, headers)
        except (urllib.error.URLError, OSError) as e:
            # jamais l'URL dans le journal
            self.log(f"notification non envoyée ({type(e).__name__})")


def github_get(url: str, token: str = "", etag: str = "") -> tuple[int, object, str]:
    """GET JSON sur l'API GitHub. Retourne (code HTTP, contenu, ETag) ; code 0 si le réseau échoue.
    Le jeton éventuel ne sert qu'à relever la limite de requêtes ; il n'est jamais journalisé.
    Une liste de releases (notes volumineuses) a une limite de lecture plus large que les avis ; trop grande : code 0."""
    limit = MAX_RELEASES_BODY if url.split("?", 1)[0].endswith("/releases") else MAX_API_BODY
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28",
               "User-Agent": "docker-update/1.0"}
    if token:
        headers["Authorization"] = "Bearer " + token
    if etag:
        headers["If-None-Match"] = etag
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=15) as r:
            return r.status, json.loads(r.read(limit)), r.headers.get("ETag", "")
    except urllib.error.HTTPError as e:
        return e.code, None, (e.headers.get("ETag", "") if e.headers else "")
    except (urllib.error.URLError, OSError, ValueError):
        return 0, None, ""


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def http_status(url: str, timeout: int = 10) -> int:
    """Code HTTP d'un GET sans suivre les redirections ; 0 si la connexion échoue."""
    opener = urllib.request.build_opener(_NoRedirect)
    req = urllib.request.Request(url, headers={"User-Agent": "docker-update/1.0"})
    try:
        with opener.open(req, timeout=timeout) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except (urllib.error.URLError, OSError, ValueError):
        return 0


def local_minutes() -> int:
    t = time.localtime()
    return t.tm_hour * 60 + t.tm_min


# ── Cœur ────────────────────────────────────────────────────────────────────

@dataclass
class Eval:
    svc: Service
    status: str  # uptodate | pending | skipped | error
    detail: str = ""
    ref: str = ""
    remote: str = ""
    old_image_id: str = ""
    first_seen: int = 0
    eligible: bool = False
    failed_before: bool = False
    fast: bool = False          # raccourci par un avis de sécurité confirmé
    urgent: bool = False        # avis critique : ignore aussi la fenêtre horaire
    sec_ids: list[str] = field(default_factory=list)
    sec_version: str = ""
    adv_pub: int = 0            # publication (epoch) de l'avis le plus récent qui justifie l'accélération


class Updater:
    def __init__(self, cfg: Config, runner: Runner, notifier: Notifier, state: State,
                 now: Callable[[], float] = time.time, sleep: Callable[[float], None] = time.sleep,
                 log: Callable[[str], None] = print, http: Callable[[str], int] = http_status,
                 minutes: Callable[[], int] = local_minutes, persist: bool = True,
                 gh_get: Callable[[str, str, str], tuple[int, object, str]] = github_get):
        self.cfg, self.run, self.notifier, self.state = cfg, runner, notifier, state
        self.now, self.sleep, self.log = now, sleep, log
        self.http, self.minutes, self.persist = http, minutes, persist
        self.gh_get = gh_get
        self.alerts_on = False

    def held(self, svc: Service) -> bool:
        holds = self.state.data["holds"]
        return svc.name in holds or (bool(svc.group) and svc.group in holds)

    def smoke(self, svc: Service) -> bool:
        if not svc.smoke_url:
            return True
        for attempt in range(3):
            if self.http(svc.smoke_url) in svc.smoke_expect:
                return True
            if attempt < 2:
                self.sleep(5)
        return False

    def save_state(self) -> None:
        if self.persist:
            self.state.save()

    # -- docker helpers ------------------------------------------------------

    def _base(self, svc: Service) -> list[str]:
        argv = ["docker", "compose", "--project-directory", svc.compose_dir]
        for f in svc.compose_files:
            argv += ["-f", f]
        return argv

    def container_id(self, svc: Service) -> str:
        r = self.run.run(self._base(svc) + ["ps", "-q", "--all", svc.service])
        if r.rc != 0:
            raise UpdateError(f"compose ps a échoué : {r.err.strip()[:200]}")
        lines = [x for x in r.out.split() if x]
        return lines[0] if lines else ""

    def container_status(self, cid: str) -> str:
        r = self.run.run(["docker", "inspect", "--format", "{{.State.Status}}", cid])
        status = clean_text(r.out.partition("|")[0], 30)
        if r.rc != 0 or not status:
            raise UpdateError("docker inspect du conteneur a échoué")
        return status

    def image_ref(self, svc: Service) -> str:
        r = self.run.run(self._base(svc) + ["config", "--format", "json"])
        if r.rc != 0:
            raise UpdateError(f"compose config a échoué : {r.err.strip()[:200]}")
        try:
            image = json.loads(r.out)["services"][svc.service]["image"]
        except (KeyError, ValueError, TypeError):
            raise UpdateError(f"service {svc.service} introuvable ou sans « image: » dans le compose")
        return str(image)

    def remote_digest(self, ref: str) -> str:
        """Digest de niveau supérieur de l'étiquette (celui qu'enregistre `docker pull`).

        buildx : on lit le manifeste brut (`--raw`, sans télécharger les attestations) et on le hache ;
        c'est exactement le digest que le registre annonce. regctl : `regctl image digest` (requête HEAD)."""
        repo, tag, _ = split_ref(ref)
        full = f"{repo}:{tag}"
        if self.cfg.settings.registry_tool == "buildx":
            rc, raw, err = self.run.run_bytes(["docker", "buildx", "imagetools", "inspect", full, "--raw"], timeout=60)
            if rc != 0 or not raw.lstrip().startswith(b"{") or len(raw) > MAX_MANIFEST:
                raise UpdateError(f"manifeste distant illisible pour {full} ({err.strip()[:150] or 'sortie inattendue'})")
            return "sha256:" + hashlib.sha256(raw).hexdigest()
        r = self.run.run(["regctl", "image", "digest", full], timeout=60)
        out = r.out.strip()
        if r.rc != 0 or not DIGEST_RE.match(out):
            raise UpdateError(f"digest distant introuvable pour {full} ({r.err.strip()[:150] or 'sortie inattendue'})")
        return out

    def local_state(self, cid: str) -> tuple[str, list[str]]:
        r = self.run.run(["docker", "inspect", "--format", "{{.Image}}", cid])
        image_id = r.out.strip()
        if r.rc != 0 or not image_id:
            raise UpdateError("docker inspect du conteneur a échoué")
        r = self.run.run(["docker", "image", "inspect", "--format", "{{json .RepoDigests}}", image_id])
        try:
            digests = json.loads(r.out) or []
        except ValueError:
            digests = []
        return image_id, [d.split("@", 1)[1] for d in digests if "@" in d]

    # -- évaluation ----------------------------------------------------------

    def evaluate(self, svc: Service) -> Eval:
        try:
            if self.held(svc):
                return Eval(svc, "skipped", "en pause (hold)")
            cid = self.container_id(svc)
            if not cid:
                return Eval(svc, "skipped", "conteneur absent")
            status = self.container_status(cid)
            if status not in RUNNING_STATES:
                # un service arrêté (volontairement) n'est touché en rien : ni registre, ni mise à jour, ni veille, ni nettoyage
                return Eval(svc, "skipped", f"conteneur arrêté ({status}), non touché")
            ref = self.image_ref(svc)
            repo, tag, pinned = split_ref(ref)
            if pinned:
                return Eval(svc, "skipped", "image épinglée par digest dans le compose (gérée à la main)", ref=ref)
            if tag in FLOATING_TAGS and svc.mode == "auto":
                self.log(f"  avertissement : {svc.name} suit l'étiquette flottante « {tag} » ; "
                         f"préférez une étiquette de version mineure")
            remote = self.remote_digest(ref)
            image_id, local = self.local_state(cid)
            if not local:
                return Eval(svc, "skipped", "image locale sans digest (construite ici ?)", ref=ref)
            if remote in local:
                return Eval(svc, "uptodate", ref=ref, remote=remote, old_image_id=image_id)
            now = int(self.now())
            rec = self.state.get(ref)
            if not rec or rec.get("digest") != remote:
                rec = {"digest": remote, "first_seen": now, "notified": False, "failed": False}
                self.state.put(ref, rec)
            first = int(rec["first_seen"])
            return Eval(svc, "pending", ref=ref, remote=remote, old_image_id=image_id, first_seen=first,
                        eligible=(now - first) >= svc.cooldown, failed_before=bool(rec.get("failed")))
        except UpdateError as e:
            return Eval(svc, "error", str(e))

    def describe(self, e: Eval) -> str:
        n = e.svc.name
        if e.status == "uptodate":
            return f"{n}: à jour"
        if e.status in ("skipped", "error"):
            return f"{n}: {'ERREUR' if e.status == 'error' else 'ignoré'} — {e.detail}"
        age = int(self.now()) - e.first_seen
        if e.failed_before:
            return f"{n}: nouvelle image {e.remote[7:19]} déjà tentée sans succès — attend une autre version ou `now`"
        sec = f" [CORRECTIF DE SÉCURITÉ {', '.join(e.sec_ids)} → {e.sec_version}]" if e.fast else ""
        if e.eligible:
            return f"{n}: nouvelle image {e.remote[7:19]} prête (vue depuis {fmt_duration(age)}), mode {e.svc.mode}{sec}"
        if e.fast:
            left = max(0, self._ready_at(e) - int(self.now()))
            return f"{n}: nouvelle image {e.remote[7:19]} différée{sec} — encore {fmt_duration(left)} (délai de sécurité)"
        left = e.svc.cooldown - age
        return f"{n}: nouvelle image {e.remote[7:19]} différée — encore {fmt_duration(left)} sur {fmt_duration(e.svc.cooldown)}"

    def _cleanup_state(self, evals: list[Eval]) -> None:
        pending_refs = {e.ref for e in evals if e.status == "pending"}
        error_refs = {e.ref for e in evals if e.status in ("error", "skipped") and e.ref}
        for ref in list(self.state.images):
            if ref not in pending_refs and ref not in error_refs:
                self.state.drop(ref)

    # -- veille de sécurité --------------------------------------------------

    def _token(self) -> str:
        f = self.cfg.settings.github_token_file
        if not f:
            return ""
        try:
            with open(f, encoding="utf-8") as fh:
                return fh.read().strip()
        except OSError:
            return ""

    def _poll_github(self, bucket: str, repo: str, path: str, compact: Callable[[object], dict | None],
                     label: str, empty: tuple[int, ...] = (), backoff: tuple[int, ...] = (),
                     meta: dict | None = None) -> list[dict]:
        """Liste GitHub d'un dépôt, en cache (ETag) : au plus une requête par `advisory_poll`.
        `empty` : codes qui signifient « rien à lire » (liste vide valide) ; `backoff` : codes après lesquels
        on attend aussi tout l'intervalle (limite de débit) au lieu de réessayer à chaque passage ;
        `meta` : champs ajoutés à l'entrée de cache quand la lecture réussit."""
        st = self.cfg.settings
        cache = self.state.data[bucket]
        rec = cache.get(repo) or {}
        now = int(self.now())
        if now - int(rec.get("fetched", 0)) < st.advisory_poll and (rec.get("ok") or rec.get("err") in backoff):
            return rec.get("items", [])
        code, body, etag = self.gh_get(f"{st.github_api}/repos/{repo}/{path}", self._token(), rec.get("etag", ""))
        if code == 304 and rec.get("ok"):
            rec.update(meta or {}, fetched=now)
        elif code == 200 and isinstance(body, list):
            rec = {"ok": True, "fetched": now, "etag": etag, "items": [c for c in (compact(a) for a in body) if c],
                   **(meta or {})}
        elif code in empty:
            rec = {"ok": True, "fetched": now, "etag": "", "items": [], **(meta or {})}
        else:
            # échec (réseau, limite de débit, dépôt sans avis publics) : on garde l'ancien contenu, on réessaie plus tard
            if not rec.get("err"):
                self.log(f"  {label} : {repo} injoignable (HTTP {code}) ; réessai plus tard")
            rec = dict(rec, fetched=now, err=code)
            if not rec.get("ok"):
                rec["items"] = []
        cache[repo] = rec
        return rec.get("items", [])

    def advisories(self, repo: str) -> list[dict]:
        """Avis publiés par le dépôt."""
        return self._poll_github("advisories", repo, "security-advisories?per_page=50", compact_advisory,
                                 "veille de sécurité")

    def releases(self, repo: str) -> list[dict]:
        """Dernières releases du dépôt (compactées) : 30 à la première lecture (historique), puis 10 seulement, car on
        ne cherche que les nouveautés ; 30 de nouveau si la dernière lecture complète a plus de 7 jours.
        404 : dépôt sans release lisible, ce n'est pas une erreur."""
        rec = self.state.data["releases"].get(repo) or {}
        now = int(self.now())
        full = not rec.get("ok") or now - int(rec.get("full", 0)) > RELEASES_RESYNC
        n = RELEASES_FIRST if full else RELEASES_NEXT
        return self._poll_github("releases", repo, f"releases?per_page={n}", compact_release, "notes de release",
                                 empty=(404,), backoff=(403, 429), meta={"full": now if full else rec.get("full", 0)})

    def _labels(self, target: str) -> dict:
        r = self.run.run(["docker", "image", "inspect", "--format", "{{json .Config.Labels}}", target])
        try:
            labels = json.loads(r.out) if r.rc == 0 else {}
        except ValueError:
            labels = {}
        return labels if isinstance(labels, dict) else {}

    def _version_of(self, target: str) -> str:
        labels = self._labels(target)
        for key in (self.cfg.settings.version_label, "org.label-schema.version"):
            v = str(labels.get(key) or "")
            if parse_version(v):
                return v.lstrip("v")
        return ""

    def _affects(self, a: dict, version: str) -> bool | None:
        """True/False, ou None si on ne peut pas conclure (plage illisible)."""
        if not a["ranges"]:
            return None
        verdict: bool | None = False
        for rng in a["ranges"]:
            try:
                if in_range(version, rng):
                    return True
            except ValueError:
                verdict = None
        return verdict

    def _ready_at(self, e: Eval) -> int:
        """Un correctif de sécurité attend `security_cooldown` après la plus tardive de deux dates : première
        observation de l'image et publication de l'avis. Un avis tout neuf ne déclenche donc rien d'instantané."""
        return max(e.first_seen, e.adv_pub) + e.svc.security_cooldown

    def alert(self, key: str | list[str], title: str, text: str, level: str = "warn",
              label: str = "ALERTE sécurité") -> None:
        """Journalise, puis notifie une seule fois par clé (plusieurs clés : un message pour le lot)."""
        self.log(f"  {label} : {title} — {text}")
        if not self.alerts_on:
            return
        keys = [key] if isinstance(key, str) else key
        alerts = self.state.data["alerts"]
        if all(k in alerts for k in keys):
            return
        for k in keys:
            alerts[k] = int(self.now())
        self.notifier.send(title, text, level)

    def security_pass(self, evals: list[Eval], allow_pull: bool) -> None:
        now = int(self.now())
        lookback = self.cfg.settings.advisory_lookback
        for e in evals:
            svc = e.svc
            if not svc.advisories or e.status not in ("pending", "uptodate") or not e.old_image_id:
                continue
            items: list[dict] = []
            for repo in svc.advisories:
                items += self.advisories(repo)
            recent = [a for a in items if not a["withdrawn"] and now - a["pub"] <= lookback
                      and sev_rank(a["sev"]) >= sev_rank(svc.min_severity)]
            if not recent:
                continue
            current = self._version_of(e.old_image_id)
            if not current:
                a = recent[0]
                self.alert(f"inconnue:{svc.name}:{a['id']}", f"Avis de sécurité récent : {svc.name}",
                           f"{a['id']} ({a['sev'] or 'sévérité ?'}) {a['summary']}. Impossible de savoir si la version en cours "
                           f"est touchée (étiquette de version absente de l'image). Vérifiez à la main.")
                continue
            affecting = [a for a in recent if self._affects(a, current)]
            unsure = [a for a in recent if self._affects(a, current) is None]
            for a in unsure:
                self.alert(f"plage:{svc.name}:{a['id']}", f"Avis de sécurité à vérifier : {svc.name}",
                           f"{a['id']} {a['summary']} — plage de versions illisible, à comparer à la version {current}.")
            if not affecting:
                continue
            names = ", ".join(f"{a['id']}{' / ' + a['cve'] if a['cve'] else ''} ({a['sev'] or '?'})" for a in affecting)
            if e.status == "uptodate":
                self.alert(f"sans-correctif:{svc.name}:{current}:{affecting[0]['id']}", f"VULNÉRABLE : {svc.name} {current}",
                           f"{names}. L'étiquette suivie ne propose pas encore d'image corrigée. "
                           f"Surveillez la publication d'une version corrigée.", "error")
                continue
            if not allow_pull:
                self.log(f"  {svc.name}: la version en cours ({current}) est touchée par {names} ; "
                         f"l'image candidate sera évaluée au prochain passage `run`")
                continue
            candidate = self._candidate_version(e)
            if not candidate:
                self.alert(f"candidate-inconnue:{svc.name}:{e.remote}:{affecting[0]['id']}",
                           f"Avis de sécurité : {svc.name} {current} touché",
                           f"{names}. Une nouvelle image existe mais sa version n'est pas lisible : je NE l'accélère PAS. "
                           f"À vous de juger : `docker-update now {svc.name} --yes`.", "error")
                continue
            if candidate == "(simulation)":
                self.log(f"  {svc.name}: simulation — l'image candidate serait téléchargée pour lire sa version")
                continue
            fixed = [a for a in affecting if self._affects(a, candidate) is False]
            if not fixed:
                self.alert(f"candidate-touchee:{svc.name}:{e.remote}:{affecting[0]['id']}",
                           f"Avis de sécurité : pas de correctif dans la nouvelle image de {svc.name}",
                           f"{names}. L'image candidate ({candidate}) est elle aussi concernée ou non confirmée : délai normal conservé.")
                continue
            e.fast, e.sec_version = True, candidate
            e.sec_ids = [a["id"] for a in fixed]
            e.adv_pub = max(a["pub"] for a in fixed)
            e.urgent = any(a["sev"] == "critical" for a in fixed)
            fixed_names = ", ".join(f"{a['id']} ({a['sev'] or '?'})" for a in fixed)
            if svc.security_policy == "fast" and svc.mode == "auto":
                if not e.failed_before and now >= self._ready_at(e):
                    e.eligible = True
                self.alert(f"correctif:{svc.name}:{e.remote}", f"Correctif de sécurité détecté : {svc.name}",
                           f"{fixed_names} corrigé dans {candidate} (en cours : {current}). Mise à jour accélérée : "
                           f"délai ramené à {fmt_duration(svc.security_cooldown)}.", "warn")
            else:
                why = "mode non automatique" if svc.mode != "auto" else "politique « alert »"
                self.alert(f"correctif:{svc.name}:{e.remote}", f"Correctif de sécurité disponible : {svc.name}",
                           f"{fixed_names} corrigé dans {candidate} (en cours : {current}). Pas de mise à jour "
                           f"automatique ({why}) : `docker-update now {svc.name} --yes` pour l'appliquer.", "warn")

    # -- veille des notes de release (ALERTES SEULEMENT) ---------------------
    # Une note de release est un texte non fiable : elle ne touche jamais au délai, à l'éligibilité ni à la fenêtre.

    def _release_version_of(self, target: str) -> str:
        labels = self._labels(target)
        for key in (self.cfg.settings.version_label, "org.label-schema.version"):
            v = str(labels.get(key) or "")
            if parse_release_version(v):
                return v
        return ""

    @staticmethod
    def _external(value, limit: int) -> str:
        """Texte venu d'une note : nettoyé, borné, et sans mention possible (« @ » neutralisé)."""
        return clean_text(value, limit).replace("@", "@\u200b")

    def release_pass(self, evals: list[Eval]) -> None:
        label = "ALERTE notes de release"
        alerts = self.state.data["alerts"]
        for e in evals:
            svc = e.svc
            if not svc.release_notes or e.status not in ("pending", "uptodate") or not e.old_image_id:
                continue
            current = self._release_version_of(e.old_image_id)
            cur = parse_release_version(current)
            if cur is None:
                self.alert(f"release-version:{svc.name}", f"Notes de release non analysées : {svc.name}",
                           f"Impossible de comparer : la version en cours est absente ou illisible (étiquette "
                           f"{self.cfg.settings.version_label}). Aucune note de release n'est lue pour ce service.",
                           "info", label)
                continue
            shown = current.lstrip("v")
            ignore = [re.compile(p, re.IGNORECASE) for p in svc.release_notes_ignore]
            newer: list[tuple[tuple, str, dict]] = []
            seen: set[str] = set()
            for repo in svc.release_notes_repos or svc.advisories:
                for r in self.releases(repo):
                    v = parse_release_version(r["tag"])
                    if r["draft"] or r["pre"] or r["tag"] in seen or v is None or v[3] == 0 or v <= cur:
                        continue
                    seen.add(r["tag"])
                    newer.append((v, repo, r))
            weak: list[tuple[str, str]] = []
            for _, repo, r in sorted(newer, key=lambda t: t[0]):
                hits = [h for h in r["hits"] if not any(p.search(h["x"]) or p.search(h["t"]) for p in ignore)]
                strong = [h for h in hits if not h["w"]]
                url = f"https://github.com/{repo}/releases/tag/{r['tag']}"
                if strong:
                    motifs = ", ".join(dict.fromkeys(self._external(h["t"], 60) for h in strong[:12]))
                    self.alert(f"release:{svc.name}:{r['tag']}",
                               f"Notes de release à lire : {svc.name} {shown} → {r['tag']}",
                               f"Motifs : {motifs}\nExtrait : « {self._external(strong[0]['x'], 160)} »\nLien : {url}\n"
                               f"Alerte seulement : le délai de mise à jour n'est pas modifié.", "warn", label)
                elif hits:
                    if not self.alerts_on or f"release-info:{svc.name}:{r['tag']}" not in alerts:
                        weak.append((r["tag"], url))
            if weak:
                lines = [f"- {t} : {u}" for t, u in weak[:10]] + ([f"… et {len(weak) - 10} autre(s)"] if len(weak) > 10 else [])
                self.alert([f"release-info:{svc.name}:{t}" for t, _ in weak],
                           f"Notes de release : {svc.name} {shown}, mot « security » dans {len(weak)} release(s)",
                           "Signal faible (le mot seul, sans CVE ni correctif annoncé) :\n" + "\n".join(lines),
                           "info", label)

    def _candidate_version(self, e: Eval) -> str:
        """Version de l'image candidate : on la télécharge (sans l'exécuter) pour lire ses étiquettes."""
        repo, _, _ = split_ref(e.ref)
        target = f"{repo}@{e.remote}"
        r = self.run.run(["docker", "pull", target], mutating=True, timeout=1800)
        if self.run.dry_run:
            return "(simulation)"
        if r.rc != 0:
            return ""
        return self._version_of(target)

    # -- application ---------------------------------------------------------

    def _up(self, svc: Service) -> bool:
        r = self.run.run(self._base(svc) + ["up", "-d", "--no-deps", "--pull", "never", "--wait",
                                            "--wait-timeout", str(svc.health_timeout), svc.service],
                         mutating=True, timeout=svc.health_timeout + 120)
        if r.rc != 0:
            self.log(f"  up a échoué pour {svc.name}: {r.err.strip()[:200]}")
            return False
        if self.run.dry_run:
            return True
        self.sleep(self.cfg.settings.settle_seconds)
        try:
            cid = self.container_id(svc)
        except UpdateError:
            return False
        if not cid:
            return False
        r = self.run.run(["docker", "inspect", "--format",
                          "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}", cid])
        status, _, health = r.out.strip().partition("|")
        ok = status == "running" and health in ("", "healthy")
        if not ok:
            self.log(f"  {svc.name}: état après démarrage = {status}/{health or '-'}")
            return False
        if not self.smoke(svc):
            self.log(f"  {svc.name}: test de fumée en échec ({svc.smoke_url})")
            return False
        return True

    def apply(self, unit: str, todo: list[Eval]) -> bool:
        names = ", ".join(e.svc.name for e in todo)
        self.log(f"mise à jour de {unit} : {names}")
        targets: dict[str, tuple[str, str, str]] = {}
        old_ids: dict[str, str] = {}
        for e in todo:
            repo, _, _ = split_ref(e.ref)
            targets.setdefault(e.ref, (repo, e.remote, e.ref))
            old_ids.setdefault(e.ref, e.old_image_id)
        for ref, (repo, digest, _) in targets.items():
            r = self.run.run(["docker", "pull", f"{repo}@{digest}"], mutating=True, timeout=1800)
            if r.rc != 0:
                msg = f"{unit}: tirage de {repo}@{digest[7:19]} impossible, rien n'a été modifié ({r.err.strip()[:150]})"
                self.log(msg)
                self.notifier.send(f"Mise à jour impossible : {unit}", msg, "error")
                return False
        for ref, (repo, digest, _) in targets.items():
            r = self.run.run(["docker", "tag", f"{repo}@{digest}", ref], mutating=True)
            if r.rc != 0:
                msg = f"{unit}: étiquetage local de {ref} impossible, rien n'a été modifié"
                self.log(msg)
                self.notifier.send(f"Mise à jour impossible : {unit}", msg, "error")
                return False
        touched: list[Eval] = []
        for e in todo:
            touched.append(e)
            if not self._up(e.svc):
                return self._fail(unit, todo, touched, old_ids, targets)
        shorts = ", ".join(f"{e.svc.name} → {e.remote[7:19]}" for e in todo)
        sec = sorted({i for e in todo for i in e.sec_ids})
        if sec:
            shorts += f"\nCorrectif de sécurité appliqué : {', '.join(sec)}"
        self.log(f"{unit}: mise à jour réussie ({shorts})")
        self.notifier.send(f"Mise à jour réussie : {unit}", shorts, "ok")
        self._cleanup(todo, old_ids)
        return True

    def _cleanup(self, todo: list[Eval], old_ids: dict[str, str]) -> None:
        """Supprime l'image précédente une fois la mise à jour réussie (comme le nettoyage de Watchtower).
        Jamais forcé : docker refuse si un conteneur utilise encore l'image ou si elle porte un autre nom ;
        ce n'est alors qu'une ligne de journal. Un échec de nettoyage n'est pas un échec de mise à jour."""
        for ref, old in old_ids.items():
            members = [e for e in todo if e.ref == ref]
            if not old or old.startswith("-") or not all(e.svc.cleanup for e in members):
                continue
            r = self.run.run(["docker", "image", "rm", old], mutating=True)
            if self.run.dry_run:
                continue
            if r.rc == 0:
                self.log(f"  nettoyage : image précédente de {ref} supprimée ({short_id(old)})")
            else:
                self.log(f"  nettoyage : image précédente de {ref} conservée ({clean_text(r.err, 120)})")

    def _fail(self, unit: str, todo: list[Eval], touched: list[Eval], old_ids: dict[str, str],
              targets: dict[str, tuple[str, str, str]]) -> bool:
        """Échec : on prévient et on s'arrête. AUCUN retour arrière automatique (une base ou une application peut avoir
        migré : défaire à l'aveugle est pire que laisser en l'état). L'image précédente reste sur le disque."""
        for ref, (_, digest, _) in targets.items():
            rec = self.state.get(ref) or {"digest": digest, "first_seen": int(self.now()), "notified": False}
            rec["failed"] = True
            self.state.put(ref, rec)
        failed = touched[-1].svc.name
        before = ", ".join(f"{e.svc.name} ({short_id(e.old_image_id)})" for e in touched)
        untouched = [e.svc.name for e in todo if e not in touched]
        msg = (f"{unit}: {failed} ne repart pas avec la nouvelle image. Aucun retour arrière automatique : "
               f"intervention manuelle. Images précédentes conservées : {before}. "
               f"Pour revenir : `docker tag <image> <référence>` puis `docker compose up -d --no-deps <service>`."
               + (f" Pas touchés : {', '.join(untouched)}." if untouched else ""))
        self.log(msg)
        self.notifier.send(f"ÉCHEC mise à jour : {unit}", msg[:1500], "error")
        return False

    # -- cycles --------------------------------------------------------------

    def units(self, evals: list[Eval]) -> dict[str, list[Eval]]:
        out: dict[str, list[Eval]] = {}
        for e in evals:
            out.setdefault(e.svc.unit, []).append(e)
        return out

    def cycle(self, apply: bool) -> int:
        evals = [self.evaluate(s) for s in self.cfg.services]
        self._cleanup_state(evals)
        self.alerts_on = apply
        if not apply:
            # diagnostic de calibrage : la veille de sécurité a besoin de la version lue dans l'image en cours
            for e in evals:
                if e.svc.advisories and e.status in ("pending", "uptodate") and e.old_image_id:
                    v = self._version_of(e.old_image_id)
                    self.log(f"  veille de sécurité {e.svc.name}: version détectée {v}" if v else
                             f"  veille de sécurité {e.svc.name}: version NON détectée (étiquette "
                             f"{self.cfg.settings.version_label} absente) — aucun avis ne pourra être évalué")
        if not apply:
            for e in evals:
                if e.svc.release_notes and e.status in ("pending", "uptodate") and e.old_image_id:
                    v = self._release_version_of(e.old_image_id)
                    self.log(f"  notes de release {e.svc.name}: version détectée {v}" if v else
                             f"  notes de release {e.svc.name}: version NON détectée (étiquette "
                             f"{self.cfg.settings.version_label} absente ou illisible) — aucune note ne pourra être comparée")
        self.security_pass(evals, allow_pull=apply)
        self.release_pass(evals)
        for e in evals:
            self.log(self.describe(e))
        failures = sum(1 for e in evals if e.status == "error")
        if not apply:
            for e in evals:
                if e.svc.smoke_url and e.status != "skipped":
                    code = self.http(e.svc.smoke_url)
                    verdict = "OK" if code in e.svc.smoke_expect else f"INATTENDU (attendu {e.svc.smoke_expect})"
                    self.log(f"  test de fumée {e.svc.name}: HTTP {code} {verdict}")
        if apply:
            minutes = self.minutes()
            for unit, members in self.units(evals).items():
                todo = [e for e in members if e.status == "pending" and e.eligible
                        and not e.failed_before and e.svc.mode == "auto"]
                if not todo:
                    continue
                if not in_window(todo[0].svc.window, minutes) and not any(e.urgent for e in todo):
                    self.log(f"{unit}: prêt mais hors de la fenêtre horaire, repris au prochain passage")
                    continue
                if any(e.urgent for e in todo) and not in_window(todo[0].svc.window, minutes):
                    self.log(f"{unit}: avis CRITIQUE, la fenêtre horaire est ignorée")
                if not self.apply(unit, todo):
                    failures += 1
            for e in evals:
                if e.status == "pending" and e.eligible and e.svc.mode == "notify":
                    rec = self.state.get(e.ref)
                    if rec and not rec.get("notified"):
                        self.notifier.send(f"Nouvelle version prête : {e.svc.name}",
                                           f"image {e.remote[7:19]} vue depuis {fmt_duration(int(self.now()) - e.first_seen)}."
                                           f" Mise à jour manuelle : `docker-update now {e.svc.name} --yes`", "info")
                        rec["notified"] = True
                        self.state.put(e.ref, rec)
            errs = {e.svc.name: e.detail for e in evals if e.status == "error"}
            fresh = {k: v for k, v in errs.items() if self.state.data["errors"].get(k) != v}
            if fresh:
                self.notifier.send("docker-update : erreurs de vérification",
                                   "\n".join(f"{k}: {v}" for k, v in fresh.items())[:1500], "warn")
            self.state.data["errors"] = errs
            horizon = int(self.now()) - 120 * 86400
            self.state.data["alerts"] = {k: v for k, v in self.state.data["alerts"].items() if v >= horizon}
        self.save_state()
        return 1 if failures else 0

    def now_cmd(self, names: list[str], yes: bool) -> int:
        selected: list[Service] = []
        for n in names:
            match = [s for s in self.cfg.services if s.name == n or s.group == n]
            if not match:
                raise UpdateError(f"inconnu : {n} (services : {', '.join(s.name for s in self.cfg.services)})")
            selected += [s for s in match if s not in selected]
        held = [s.name for s in selected if self.held(s)]
        if held:
            raise UpdateError(f"en pause : {', '.join(held)} (utilisez d'abord `unhold`)")
        evals = [self.evaluate(s) for s in selected]
        for e in evals:
            self.log(self.describe(e))
        todo_all = [e for e in evals if e.status == "pending"]
        if not todo_all:
            self.log("rien à mettre à jour")
            self.save_state()
            return 0
        if not yes:
            self.log("simulation : ajoutez --yes pour appliquer (le délai est ignoré pour ces services)")
            return 0
        failures = 0
        for unit, members in self.units(todo_all).items():
            if not self.apply(unit, members):
                failures += 1
        self.save_state()
        return 1 if failures else 0

    def hold(self, names: list[str], on: bool) -> int:
        known = {s.name for s in self.cfg.services} | {s.group for s in self.cfg.services if s.group}
        for n in names:
            if n not in known:
                raise UpdateError(f"inconnu : {n}")
        holds = self.state.data["holds"]
        for n in names:
            if on and n not in holds:
                holds.append(n)
            if not on and n in holds:
                holds.remove(n)
        self.save_state()
        self.log("en pause : " + (", ".join(holds) or "(rien)"))
        return 0

    def heartbeat(self) -> None:
        f = self.cfg.settings.heartbeat_url_file
        if not f or self.run.dry_run:
            return
        try:
            with open(f, encoding="utf-8") as fh:
                url = fh.read().strip()
        except OSError:
            return
        if url and not 200 <= self.http(url) < 400:
            self.log("signal de vie non reçu par le service de surveillance")

    def status(self) -> None:
        if self.state.data["holds"]:
            self.log("en pause : " + ", ".join(self.state.data["holds"]))
        if not self.state.images:
            self.log("aucune image en attente")
        for ref, rec in sorted(self.state.images.items()):
            age = int(self.now()) - int(rec.get("first_seen", 0))
            flags = " [échec]" if rec.get("failed") else ""
            self.log(f"{ref} → {rec.get('digest', '')[7:19]} vue depuis {fmt_duration(age)}{flags}")


# ── Programme principal ─────────────────────────────────────────────────────

def validate(cfg: Config, log: Callable[[str], None] = print) -> int:
    problems = 0
    tools = ["docker"] + (["regctl"] if cfg.settings.registry_tool == "regctl" else [])
    for t in tools:
        if not shutil.which(t):
            log(f"ERREUR : {t} introuvable")
            problems += 1
    for s in cfg.services:
        if not os.path.isdir(s.compose_dir):
            log(f"ERREUR : {s.name}: dossier compose introuvable : {s.compose_dir}")
            problems += 1
        for f in s.compose_files:
            if not os.path.isfile(f):
                log(f"ERREUR : {s.name}: fichier compose introuvable : {f}")
                problems += 1
    nu = cfg.settings.notify_url_file
    if nu:
        if not os.path.isfile(nu):
            log(f"AVERTISSEMENT : fichier d'URL de notification absent : {nu} (aucune notification)")
        elif os.stat(nu).st_mode & 0o077:
            log(f"AVERTISSEMENT : {nu} est lisible par d'autres comptes (chmod 600)")
    log(f"{len(cfg.services)} service(s), {len({s.unit for s in cfg.services})} unité(s) : "
        f"{'OK' if not problems else str(problems) + ' problème(s)'}")
    return 1 if problems else 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="docker-update", description=__doc__.split("\n")[0])
    ap.add_argument("--config", default="/etc/docker-update/updates.toml")
    ap.add_argument("--dry-run", action="store_true", help="n'exécute aucune commande qui modifie quelque chose")
    ap.add_argument("--allow-untrusted-config", action="store_true",
                    help="ne pas exiger une configuration appartenant à root (tests uniquement)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("validate")
    sub.add_parser("check")
    sub.add_parser("run")
    sub.add_parser("status")
    sub.add_parser("notify-test")
    p_hold = sub.add_parser("hold")
    p_hold.add_argument("names", nargs="+")
    p_unhold = sub.add_parser("unhold")
    p_unhold.add_argument("names", nargs="+")
    p_now = sub.add_parser("now")
    p_now.add_argument("names", nargs="+")
    p_now.add_argument("--yes", action="store_true")
    args = ap.parse_args(argv)

    def log(msg: str) -> None:
        print(msg, flush=True)

    try:
        if not args.allow_untrusted_config:
            check_trusted_file(args.config)
        cfg = load_config(args.config)
        if args.cmd == "validate":
            return validate(cfg, log)
        lock = open(cfg.settings.lock_file, "w")
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            log("un autre passage est en cours, rien à faire")
            return 0
        runner = Runner(dry_run=args.dry_run, log=log)
        notifier = Notifier(cfg.settings.notify_url_file, cfg.settings.notify_format, log, dry_run=args.dry_run)
        up = Updater(cfg, runner, notifier, State(cfg.settings.state_file), log=log, persist=not args.dry_run)
        if args.cmd == "check":
            return up.cycle(apply=False)
        if args.cmd == "run":
            rc = up.cycle(apply=True)
            if rc == 0:
                up.heartbeat()
            return rc
        if args.cmd == "now":
            return up.now_cmd(args.names, args.yes)
        if args.cmd == "notify-test":
            if not notifier._url():
                log("aucune URL de notification lisible (notify_url_file)")
                return 1
            notifier.send("docker-update : test", "Si vous lisez ceci, les notifications fonctionnent.", "info")
            log("message de test envoyé (ou signalé ci-dessus en cas d'échec)")
            return 0
        if args.cmd in ("hold", "unhold"):
            return up.hold(args.names, args.cmd == "hold")
        up.status()
        return 0
    except ConfigError as e:
        log(f"configuration : {e}")
        return 2
    except UpdateError as e:
        log(f"erreur : {e}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
