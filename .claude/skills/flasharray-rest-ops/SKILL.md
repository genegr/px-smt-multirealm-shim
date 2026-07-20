---
name: flasharray-rest-ops
description: Purity//FA REST v2 recipes for realms/pods tenancy — login, realm-scoped API tokens (policy+admin+token), granting array-level (global) hosts access to realm volumes via resource-accesses, creating/renaming/deleting hosts/realms/pods/volumes (with the correct destroy+eradicate order), and per-FQDN array-identity gotchas. Use when scripting Pure FlashArray provisioning/cleanup for Portworx FADA or multi-realm setups.
---

# FlashArray (Purity//FA) REST v2 recipes

Base `https://<array>/api/<ver>`. Discover versions: `GET /api/api_version` (the list may end
with a `"2.X"` sentinel — the real max is the numeric one before it, e.g. `2.54`).

## Auth

```
POST /api/2.x/login   header: api-token: <token>   → response header x-auth-token: <session>
```
Use `x-auth-token: <session>` on every subsequent call. Re-login on 401.

## Realm-scoped API token (what Portworx `pure.json` uses per realm)

A realm-scoped token is a **local admin** whose only permission is a **management-access
policy** scoped to one realm:

1. `POST /policies/management-access?names=<realm>-pol` body
   `{"enabled":true,"aggregation_strategy":"all-permissions","rules":[{"scope":{"resource_type":"realms","name":"<realm>"},"role":{"name":"storage"}}]}`
2. `POST /admins?names=eg-<realm>` body `{"password":"<any-strong>","management_access_policies":[{"name":"<realm>-pol"}]}`
   (a local admin **requires** a password even if only the token is used).
3. `POST /admins/api-tokens?names=eg-<realm>` → the token value is returned **once**, here only.

Such a token sees/addresses **only** its realm's objects; it cannot see array-level (non-realm)
hosts (a realm token returns "Host does not exist" for them). That scoping is exactly why
Portworx can't attach realm volumes to array-level hosts without help.

## Grant an array-level (global) host access to a realm volume (the key feature)

Needed so a non-realm host can connect a realm/pod volume.
- **CLI:** `purehost access create --realm <realm> <global-host>`
- **REST (Purity 6.8+ / newer 2.x):** `POST /api/2.x/resource-accesses/batch` body (array):
  ```json
  [ { "resource": {"name":"<global-host>","resource_type":"hosts"},
      "scope":    {"name":"<realm>","resource_type":"realms"} } ]
  ```
  ⚠️ Older Purity (e.g. 6.7.x / REST ≤2.36) does **not** expose `resource-accesses` (404) — the
  array must be new enough (6.10.x confirmed working). This is a one-time provisioning grant.

## Create / rename objects

- Host: `POST /hosts?names=<h>` then `PATCH /hosts?names=<h>` `{"iqns":["iqn…"]}`.
- Realm: `POST /realms?names=<realm>`. Pod in realm: `POST /pods?names=<realm>::<pod>`.
- Rename a pod (empty): `PATCH /pods?names=<realm>::<old>` `{"name":"<realm>::<new>"}`.
- Connect volume↔host: `POST /connections?host_names=<h>&volume_names=<realm>::<pod>::<vol>`.
- URL-encode `::` in query params; operate on **one realm's objects per call** (the API rejects
  mixing objects from different realms/arrays: "Operation can only specify objects from a single
  array or realm").

## Delete order (destroy + eradicate)

Purity uses soft-destroy then eradicate. Respect dependencies or you get 400s:

- **Volume:** delete its connections (`DELETE /connections?host_names=&volume_names=`) →
  `PATCH /volumes?names=` `{"destroyed":true}` → `DELETE /volumes?names=` (eradicate).
- **Pod:** a fresh pod auto-creates `<pod>::pgroup-auto` pinned to the container **default
  protection list**. To remove the pod: clear the pod's default protection
  (`container-default-protections`, keyed by `names=<pod>`, set `default_protections: []`) →
  destroy+eradicate the pgroup → destroy+eradicate the pod. (Often simplest to just keep the
  pod and only clean its volumes.)
- **Realm:** must be empty (pods destroyed) first, then destroy+eradicate the realm.
- **Host:** delete its connections first, then `DELETE /hosts?names=`.
- Eradication requires the realm's `eradication_config` to allow manual eradicate
  (`manual_eradication: all-enabled`).

## Multi-realm-via-one-array gotcha: array identity

If several DNS names all point at the same physical array (the multi-realm trick), `GET /arrays`
returns the **same array name+id** for all of them, so a client that de-duplicates backends by
array id (Portworx does) collapses them and mis-routes volumes across realms. A proxy in front
must present a **distinct synthetic array name+id per hostname** (and map it back on requests)
so each looks like a separate array.

## Safety

FlashArrays are often shared across tenants/realms. **Only ever delete objects you created**,
and scope deletes by exact name prefix (`<realm>::…`). Never match loosely (e.g. a bare
`::worker` glob) — it can catch another tenant's objects.
