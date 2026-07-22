#!/usr/bin/env python3
"""Reset a FlashArray to a declarative realm/pod/host/grant/token spec for the px-smt topology
test. Reads a JSON spec (path arg or stdin), purges the named old realms + their volumes/pods,
reshapes array-level hosts, then ensures the target realms/pods/hosts/grants/realm-tokens.

Idempotent where practical; "already exists" is treated as success. Realm-scoped tokens are
written to  ~/.<tag>-<realm>-token  (mode 600) and echoed (name only) at the end.

Spec fields:
  host, token_file, tag
  purge_realms       [str]           realms to fully delete (with their pods/volumes/pgroups)
  purge_admins       [str]           admin accounts to delete
  purge_policies     [str]           management-access policies to delete
  remove_hosts       [str]           array-level hosts to delete (connections cleared first)
  rename_hosts       {old: new}      rename array-level hosts
  array_hosts        {name: [wwns]}  ensure these hosts exist with these FC WWNs
  realms             [str]           realms to ensure
  pods               {realm: pod}    one pod per realm to ensure (realm::pod)
  admins             {realm: admin}  realm-scoped admin (+ <realm>-pol policy + api-token) to ensure
"""
import json, os, ssl, sys, urllib.error, urllib.parse, urllib.request

CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE
VER = "2.40"


class FA:
    def __init__(self, host, token):
        self.base = f"https://{host}/api/{VER}"
        self.token = token
        req = urllib.request.Request(self.base + "/login", method="POST",
                                     headers={"api-token": token})
        with urllib.request.urlopen(req, context=CTX) as r:
            self.sess = r.headers.get("x-auth-token")
        if not self.sess:
            raise SystemExit(f"login failed on {host}")

    def call(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        headers = {"x-auth-token": self.sess}
        if data:
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, context=CTX) as r:
                return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"{}")
            except Exception:
                return e.code, {}

    def names(self, resource, **q):
        qs = ("?" + urllib.parse.urlencode(q)) if q else ""
        _, d = self.call("GET", f"/{resource}{qs}")
        return [i.get("name") for i in d.get("items", [])]


def q(name):
    return urllib.parse.quote(name, safe="")


def purge_volume(fa, vol):
    # delete every connection to the volume, then destroy + eradicate it
    _, d = fa.call("GET", f"/connections?volume_names={q(vol)}")
    for c in d.get("items", []):
        h = c.get("host", {}).get("name")
        if h:
            fa.call("DELETE", f"/connections?host_names={q(h)}&volume_names={q(vol)}")
    fa.call("PATCH", f"/volumes?names={q(vol)}", {"destroyed": True})
    s, r = fa.call("DELETE", f"/volumes?names={q(vol)}")
    print(f"    volume {vol}: eradicate -> {s}")


def purge_pod(fa, pod):
    # a fresh pod pins <pod>::pgroup-auto to the container default protection; clear it, then
    # destroy+eradicate the pgroup, then the pod
    fa.call("PATCH", f"/container-default-protections?names={q(pod)}", {"default_protections": []})
    pg = f"{pod}::pgroup-auto"
    fa.call("PATCH", f"/protection-groups?names={q(pg)}", {"destroyed": True})
    fa.call("DELETE", f"/protection-groups?names={q(pg)}")
    fa.call("PATCH", f"/pods?names={q(pod)}", {"destroyed": True})
    s, _ = fa.call("DELETE", f"/pods?names={q(pod)}")
    print(f"    pod {pod}: eradicate -> {s}")


def main():
    spec = json.load(open(sys.argv[1]) if len(sys.argv) > 1 else sys.stdin)
    fa = FA(spec["host"], open(os.path.expanduser(spec["token_file"])).read().strip())
    tag = spec["tag"]
    print(f"== {tag} ({spec['host']}) ==")

    # ---- CLEAN LEFTOVERS ----
    # Delete Portworx-created volumes + realm-scoped hosts under these realms WITHOUT touching the
    # realm/pod/array-host/token structure. Used on teardown to return to a redeployable baseline
    # (the realms themselves are SafeMode-ratcheted and cannot be eradicated anyway).
    for r in spec.get("clean_leftovers", []):
        _, dv = fa.call("GET", "/volumes")
        for v in dv.get("items", []):
            if v["name"].startswith(r + "::"):
                purge_volume(fa, v["name"])
        for h in fa.names("hosts"):
            if h and "::" in h and h.startswith(r + "::"):
                fa.call("DELETE", f"/hosts?names={q(h)}")
                print(f"    realm-host {h}: deleted")

    # ---- PURGE ----
    purge_realms = spec.get("purge_realms", [])
    if purge_realms:
        # volumes first (anything under a purge realm prefix, incl. leftover PX drives)
        _, dv = fa.call("GET", "/volumes")
        for v in dv.get("items", []):
            if any(v["name"].startswith(r + "::") for r in purge_realms):
                purge_volume(fa, v["name"])
        # leftover realm-scoped hosts (name contains ::) under purge realms
        for h in fa.names("hosts"):
            if h and "::" in h and any(h.startswith(r + "::") for r in purge_realms):
                fa.call("DELETE", f"/hosts?names={q(h)}")
                print(f"    realm-host {h}: deleted")
        # pods, then realms
        for r in purge_realms:
            for p in fa.names("pods"):
                if p and p.startswith(r + "::"):
                    purge_pod(fa, p)
            fa.call("PATCH", f"/realms?names={q(r)}", {"destroyed": True})
            s, _ = fa.call("DELETE", f"/realms?names={q(r)}")
            print(f"    realm {r}: eradicate -> {s}")
    for a in spec.get("purge_admins", []):
        fa.call("DELETE", f"/admins?names={q(a)}")
    for p in spec.get("purge_policies", []):
        fa.call("DELETE", f"/policies/management-access?names={q(p)}")

    # ---- HOSTS ----
    for h in spec.get("remove_hosts", []):
        _, d = fa.call("GET", f"/connections?host_names={q(h)}")
        for c in d.get("items", []):
            v = c.get("volume", {}).get("name")
            if v:
                fa.call("DELETE", f"/connections?host_names={q(h)}&volume_names={q(v)}")
        s, _ = fa.call("DELETE", f"/hosts?names={q(h)}")
        print(f"  host {h}: removed -> {s}")
    for old, new in spec.get("rename_hosts", {}).items():
        s, _ = fa.call("PATCH", f"/hosts?names={q(old)}", {"name": new})
        print(f"  host {old} -> {new}: {s}")
    for name, wwns in spec.get("array_hosts", {}).items():
        if name not in fa.names("hosts"):
            fa.call("POST", f"/hosts?names={q(name)}")
        fa.call("PATCH", f"/hosts?names={q(name)}", {"wwns": wwns})
        print(f"  host {name}: wwns set")

    # ---- ENSURE realms / pods ----
    existing_realms = set(fa.names("realms"))
    for r in spec.get("realms", []):
        if r not in existing_realms:
            fa.call("POST", f"/realms?names={q(r)}")
        # allow manual eradicate so future teardown can eradicate cleanly
        fa.call("PATCH", f"/realms?names={q(r)}",
                {"eradication_config": {"manual_eradication": "all-enabled"}})
        print(f"  realm {r}: ensured")
    for r, pod in spec.get("pods", {}).items():
        full = f"{r}::{pod}"
        if full not in fa.names("pods"):
            fa.call("POST", f"/pods?names={q(full)}")
        print(f"  pod {full}: ensured")

    # ---- GRANTS: every array_host into every realm ----
    for r in spec.get("realms", []):
        batch = [{"resource": {"name": h, "resource_type": "hosts"},
                  "scope": {"name": r, "resource_type": "realms"}}
                 for h in spec.get("array_hosts", {})]
        if batch:
            fa.call("POST", "/resource-accesses/batch", batch)
            print(f"  grants into {r}: {list(spec.get('array_hosts', {}))}")

    # ---- ADMINS + POLICIES + TOKENS ----
    # Idempotent: if the realm-scoped token file already exists (a prior provision), reuse it —
    # re-issuing would rotate the token and break the deployed pure.json.
    for r, adm in spec.get("admins", {}).items():
        out = os.path.expanduser(f"~/.{tag}-{r}-token")
        if os.path.exists(out) and os.path.getsize(out) > 0:
            print(f"  token {adm} ({r}) already present at {out} -> reusing")
            continue
        pol = f"{r}-pol"
        fa.call("POST", f"/policies/management-access?names={q(pol)}", {
            "enabled": True, "aggregation_strategy": "all-permissions",
            "rules": [{"scope": {"resource_type": "realms", "name": r},
                       "role": {"name": "storage"}}]})
        fa.call("POST", f"/admins?names={q(adm)}", {
            "password": "PxShimDev-" + os.urandom(8).hex(),
            "management_access_policies": [{"name": pol}]})
        s, d = fa.call("POST", f"/admins/api-tokens?names={q(adm)}")
        items = d.get("items", [])
        tok = (items[0].get("api_token", {}) if items else {}).get("token")
        if tok:
            fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
            os.write(fd, tok.encode())
            os.close(fd)
            print(f"  token {adm} ({r}) -> {out}")
        else:
            print(f"  token {adm} ({r}) FAILED: {d.get('errors')}")

    print(f"== {tag} done ==")


if __name__ == "__main__":
    main()
