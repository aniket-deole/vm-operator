# Data Model: Tag CRD + Tag Controller for Affinity

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Epic**: vmop-3882

This document is the API contract for the `Tag` resource introduced by this feature: its schema, its derived metadata, its lifecycle invariants, its admission rules, and the RBAC it needs.

---

## Group, version, kind

| | |
|---|---|
| **Group** | `vsphere.policy.vmware.com` |
| **Version** | `v1alpha1` |
| **Kind** | `Tag` (list kind `TagList`) |
| **Scope** | Namespaced |
| **Go module** | `github.com/vmware-tanzu/vm-operator/external/vsphere-policy` |
| **Go package** | `external/vsphere-policy/api/v1alpha1`, alias `vspherepolv1` |
| **Source file** | `external/vsphere-policy/api/v1alpha1/tag_types.go` |
| **Status subresource** | yes |
| **Storage version** | yes (only version) |
| **Generated CRD** | `config/crd/external-crds/vsphere.policy.vmware.com_tags.yaml` (via `make generate-external-manifests`) |
| **Conversion** | none — single version, no conversion webhook (see "Conversion strategy") |

`Tag` and `TagList` append themselves to the package's `objectTypes` slice from an `init()`, exactly as `TagPolicy` does, so `AddToScheme` picks them up with no change to `groupversion_info.go`.

---

## Example

```yaml
apiVersion: vsphere.policy.vmware.com/v1alpha1
kind: Tag
metadata:
  namespace: my-namespace-1
  name: tag-95f9d49177c788e80     # "tag-" + SHA1Sum17("app:nginx")
  labels:
    app: nginx                    # mirror of spec.label/spec.value for selector queries
  ownerReferences:                # owning VMs: carry the label AND reference it from spec.affinity
    - apiVersion: vmoperator.vmware.com/v1alpha6
      kind: VirtualMachine
      name: vm-a
      uid: 8a7f2c31-0e14-4a37-9c66-1b1f0d3a55e2
      controller: false
      blockOwnerDeletion: false
  finalizers:
    - vmoperator.vmware.com/tag
spec:
  label: app                      # the label KEY the tag represents
  value: nginx                    # the label VALUE the tag represents
  serverID: 5b271c67-cf08-435e-8e30-2135bb849df5   # target vCenter GUID (forward-compat)
status:
  id: ""                          # reserved for the resolved vCenter tag UUID; NOT populated by this feature
  observedGeneration: 1
  conditions:
    - type: Ready
      status: "True"
      reason: Ready
      lastTransitionTime: "2026-08-04T10:11:12Z"
```

---

## Schema

### `TagSpec`

| Field | JSON | Type | Marker | Description |
|-------|------|------|--------|-------------|
| `Label` | `label` | `string` | `+required`, `MinLength=1` | The label **key** the tag represents (e.g. `app`). Immutable after create. Mirrored into `metadata.labels` as a key. |
| `Value` | `value` | `string` | `+required` | The label **value** the tag represents (e.g. `nginx`). Immutable after create. Mirrored into `metadata.labels[spec.label]`. An empty value is legal (see "Empty label value" below), so **no** `MinLength`. |
| `ServerID` | `serverID,omitempty` | `string` | `+optional` | GUID of the target vCenter. Recorded for forward-compatibility; a single vCenter is assumed by this feature (spec NG2). |

`spec.label` and `spec.value` together are the resource's identity: they determine the resource name (see "Name derivation"), so a change to either would make the name wrong. Both are enforced immutable by the validating webhook.

### `TagStatus`

| Field | JSON | Type | Marker | Description |
|-------|------|------|--------|-------------|
| `ID` | `id,omitempty` | `string` | `+optional` | Reserved for the resolved vCenter Tag UUID. **Not populated by this feature** (spec NG1) — the `Tag` controller performs no vCenter work, and tag attachment uses name+category rather than the UUID. Remains empty; absence is tolerated by every consumer. Retained on the CRD for forward-compatibility. |
| `ObservedGeneration` | `observedGeneration,omitempty` | `int64` | `+optional` | Last reconciled `metadata.generation` (constitution requirement). |
| `Conditions` | `conditions,omitempty` | `[]metav1.Condition` | `+optional`, `+listType=map`, `+listMapKey=type` | Standard conditions. MUST include `Ready` (`vspherepolv1.ReadyConditionType`, already defined in `common_types.go`). |

### Derived metadata

| Field | Written by | Description |
|-------|-----------|-------------|
| `metadata.name` | VM reconcile path, on create | `"tag-" + pkgutil.SHA1Sum17("<spec.label>:<spec.value>")`. See "Name derivation". |
| `metadata.namespace` | VM reconcile path, on create | The VM's namespace. Doubles as the vCenter Tag Category. |
| `metadata.labels[<spec.label>]` | VM reconcile path, on create | Mirror of `spec.value`, so an admin can run `kubectl get tags -l app=nginx`. This mirror is **not** what makes the controllers' queries efficient — see "Query surface" below. |
| `metadata.ownerReferences` | VM reconcile path — each VM writes **only its own entry** | One non-controller reference per owning VM (`controller: false`, `blockOwnerDeletion: false`). An owner is a VM that carries the label **and** references it from its own `spec.affinity`. A VM that merely carries the label is tagged in vCenter but is **not** an owner — and a VM that references the label without carrying it is neither. Ownership is not the same predicate as tag carriage; see the note below. |
| `metadata.finalizers` | `Tag` controller | `vmoperator.vmware.com/tag`. Held so that label-only VMs are enqueued to drop the vCenter tag before the `Tag` disappears (spec G5). |

### Printer columns

Per D9 in [`research.md`](./research.md), no owner column:

```go
// +kubebuilder:printcolumn:name="Label",type="string",JSONPath=".spec.label"
// +kubebuilder:printcolumn:name="Value",type="string",JSONPath=".spec.value"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
```

This satisfies spec G10 and US4 scenario 1. Owner detail stays available via `kubectl get tag <name> -o yaml` (US4 scenario 2).

### Type markers

```go
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:storageversion:true
// +kubebuilder:subresource:status
```

Matching `TagPolicy`'s marker set, plus the printer columns above.

---

## Name derivation

```
name = "tag-" + pkgutil.SHA1Sum17(spec.label + ":" + spec.value)
```

- `pkgutil.SHA1Sum17` (`pkg/util/hash.go`) returns the first 17 hex characters of a SHA-1 sum, so the name is always 21 characters, always DNS-subdomain-safe, and independent of the shape of the key and value. This mirrors the existing `VMIName` precedent (`"vmi-" + SHA1Sum17(...)`).
- The hashed string uses `":"` as the separator — the same separator as the vCenter tag name (see below) — so one canonical string identifies the pair everywhere.
- The derivation is a pure function of the key/value pair, so any VM in the namespace that needs the `Tag` computes the same name and either finds the existing resource or creates it. No lookup-by-selector is needed on the write path; the label mirror exists for read-side and diagnostic queries.
- **Collision handling**: 17 hex characters is 68 bits. Within one namespace's `Tag` set, collision is not a practical concern, and a collision would be self-consistent rather than silently wrong only if the pair matched — so on create-or-adopt the reconciler MUST verify that an existing resource with the derived name has the expected `spec.label` and `spec.value`, and MUST fail the reconcile with a wrapped error if it does not, rather than adopting a mismatched resource.

### Empty label value

`app=""` is a legal Kubernetes label. It yields `SHA1Sum17("app:")`, a `Tag` distinct from every non-empty value for the same key, and a `metadata.labels["app"] = ""` mirror — all legal. Hence no `MinLength` on `spec.value`.

---

## vCenter-side identity

| | |
|---|---|
| **Tag name** | `"<spec.label>:<spec.value>"` — byte-for-byte what `affinity.go` emits today |
| **Tag category** | `<metadata.namespace>` |
| **Emitted as** | `vimtypes.TagSpec{ArrayUpdateSpec{Operation: add\|remove}, Id: TagId{NameId: &TagIdNameId{Tag: name, Category: category}}}` |
| **Created by** | `vpxd`, when named in the VM's reconfigure request; or assumed pre-existing (spec NG1) |

Keeping this format identical to today's (D6) is what makes the emitted tags interchangeable with the Compute Policy side, which builds `TagId.NameId` the same way in `buildTagIDsFromTopology`.

---

## Query surface

The resource is designed so that "does a `Tag` for this label exist?" and "give me the `Tag`s for this label" are both answerable without scanning the namespace. Three mechanisms, in order of preference:

1. **The derived name is the primary key.** `metadata.name` is a pure function of `spec.label` + `spec.value` (see "Name derivation"), so an exact-pair existence check is a `Get` — an O(1) point read against the cache, no list and no selector. This is the check the VM reconcile path runs once per owned pair, and it is by far the most frequent query in the feature.
2. **Field indexes** for the cases where the pair is not fully known up front, registered by the `Tag` controller's `AddToManager` against the shared manager cache and therefore available to every client built from it:

   | Index name | Indexed value | Answers |
   |------------|---------------|---------|
   | `spec.label` | `spec.label` | give me every `Tag` for this label **key**, any value |
   | `spec.labelKeyValue` | `spec.label + ":" + spec.value` | give me the `Tag` for this exact pair when the name has not been derived |
   | `metadata.ownerReferences.uid` | one entry per owner UID | which `Tag`s does this VM own |

   The reverse direction — "which VMs carry this `Tag`'s label?", which the `Tag` controller asks on every create, ownership change, and delete — is indexed on the **VM** side rather than recorded on the `Tag`: a multi-valued field index `metadata.labels.keyValue` on `vmopv1.VirtualMachine`. See `plan.md` "Field indexes and query patterns". Nothing about the tagged-VM set is persisted on the `Tag` itself, which is what keeps the resource free of a second source of truth that could drift from the VMs' actual labels.

3. **The `metadata.labels` mirror** for human and `kubectl` use (`kubectl get tags -l app=nginx`, `-l app`). It is deliberately **not** used by the controllers: controller-runtime's cache maintains field indexes only, so `client.MatchingLabels` is an in-memory filter applied after the cache has returned every object in the namespace. The mirror is a convenience surface, not an index.

The mirror is still worth maintaining even though nothing in the control plane queries by it: it is the only way an admin can select `Tag`s by label from `kubectl`, and it makes a `Tag`'s meaning legible in a raw `get -o yaml` without decoding the hashed name.

## Ownership vs. tag carriage

Two different predicates, both defined in `spec.md` "Ownership vs. tag carriage", restated here because the schema above touches both:

| | Predicate | Recorded in |
|---|-----------|-------------|
| **Ownership** | VM carries the label **AND** its own `spec.affinity` references it | `metadata.ownerReferences` |
| **Tag carriage** | VM carries the label **AND** a `Tag` for the pair exists in the namespace | vCenter only — nothing on the `Tag` records which VMs carry it |

Tag carriage does not consult the VM's `spec.affinity`, so the owner set is a **subset** of the set of tagged VMs, never equal to it in the general case. Nothing on the `Tag` enumerates the tagged VMs: that set is recomputed from live state on each VM's own reconcile, which is what keeps the resource small and free of a second source of truth.

## Lifecycle invariants

1. **Creation**: created by the VM reconcile path, always with at least one owner reference (the VM whose reconcile created it) and always with its label mirror set.
2. **Ownership add**: a VM adds **only its own** owner-reference entry, when it both carries the label and references it from `spec.affinity`.
3. **Ownership remove**: a VM removes **only its own** entry, when either condition stops holding, or when the VM is being deleted (before its finalizer is released).
4. **Concurrent ownership writes**: every ownership write is a patch built with `client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})`, skipped entirely when `apiequality.Semantic.DeepEqual` reports no change to `ownerReferences`. A conflict is returned and retried by the next reconcile. A plain merge patch is **not** acceptable: `ownerReferences` is a list field and a merge patch would replace it wholesale, silently dropping a concurrent writer's entry.
5. **Deletion at zero owners**: the `Tag` controller deletes the `Tag` when it observes an empty `ownerReferences` list. Ordinary owner-reference garbage collection is a backstop that covers only the "all owner VMs deleted" case — it does **not** delete an object whose owner list was emptied while those owners still exist. See [`research.md`](./research.md) "Owner-reference garbage collection".
6. **Deletion fan-out**: before releasing `vmoperator.vmware.com/tag`, the `Tag` controller enqueues every VM in the namespace carrying `spec.label=spec.value` — via the `metadata.labels.keyValue` index, not a namespace scan — so label-only VMs drop the vCenter tag (spec G5).
7. **Re-create during deletion**: while a `Tag` with the derived name exists with a non-zero `deletionTimestamp`, a VM that needs it MUST NOT adopt it and MUST NOT skip tagging; the reconcile requeues until the delete completes and the create succeeds (spec G12).
8. **Status ownership**: only the `Tag` controller writes `status`, via the status subresource, with the same optimistic-lock-and-skip-if-unchanged discipline.
9. **No vCenter work**: nothing in the `Tag`'s lifecycle calls vCenter. `status.id` stays empty (spec NG1).

---

## Status conditions

| Type | Status | Reason | Meaning |
|------|--------|--------|---------|
| `Ready` | `True` | `Ready` | The `Tag` has been observed at its current generation, its label mirror matches its spec, and its owner set is non-empty. |
| `Ready` | `False` | `NoOwners` | The owner-reference list is empty; the `Tag` is being deleted. Transient. |
| `Ready` | `False` | `LabelMirrorMismatch` | `metadata.labels[spec.label]` does not equal `spec.value` and could not be corrected. |

`Ready` is required by the constitution ("Controllers must track `status.observedGeneration` and set a `Ready` condition"). The condition type constant is the pre-existing `vspherepolv1.ReadyConditionType` in `common_types.go`; the reason strings are new constants declared in `tag_types.go`.

---

## Admission rules (validating webhook)

Located at `webhooks/vspherepolicy/tag/validation/tag_validator.go` (D8). Verbs: `CREATE`, `UPDATE`, `DELETE`.

| # | Rule | Operation | Error |
|---|------|-----------|-------|
| V1 | `spec.label` MUST be non-empty and a valid Kubernetes label key | CREATE, UPDATE | `field.Invalid(spec.label, …)` |
| V2 | `spec.value` MUST be a valid Kubernetes label value (empty permitted) | CREATE, UPDATE | `field.Invalid(spec.value, …)` |
| V3 | `spec.label` MUST NOT change | UPDATE | `field.Forbidden(spec.label, "field is immutable")` |
| V4 | `spec.value` MUST NOT change | UPDATE | `field.Forbidden(spec.value, "field is immutable")` |
| V5 | `metadata.name` MUST equal the derived name for `spec.label`/`spec.value` | CREATE | `field.Invalid(metadata.name, …)` |
| V6 | Only privileged accounts may create, update, or delete a `Tag` | CREATE, UPDATE, DELETE | `field.Forbidden(…, "only privileged users may …")` |

V6 uses the existing `pkgctx.WebhookRequestContext.IsPrivilegedAccount` mechanism, which is what admits the VM Operator service account and CSP admins while rejecting DevOps users (spec G9, US4 scenarios 3-4).

Note on V5: it is a consistency check, not a security control — the name derivation is deterministic, so a hand-written `Tag` with a mismatched name would be invisible to the VM reconcile path (which looks the resource up by derived name) and would linger as garbage.

---

## RBAC

The VM controller's role needs write access to `Tag` (it creates them and patches ownership); the `Tag` controller needs the standard controller set plus status, **and** read access to `VirtualMachine` for its fan-out `List`:

```go
// On both controllers/virtualmachine/virtualmachine/virtualmachine_controller.go
// and controllers/vspherepolicy/tag/tag_controller.go:
// +kubebuilder:rbac:groups=vsphere.policy.vmware.com,resources=tags,verbs=get;list;watch;create;update;patch;delete

// On controllers/vspherepolicy/tag/tag_controller.go only:
// +kubebuilder:rbac:groups=vsphere.policy.vmware.com,resources=tags/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vmoperator.vmware.com,resources=virtualmachines,verbs=get;list;watch
```

The `virtualmachines` read marker is easy to overlook because the VM controller already has it — but the `Tag` controller is a separate reconciler in the same manager, and its fan-out (`List` VMs by the label index) fails without it. The manager shares one role, so the effective permission set is a union; the marker still belongs on the `Tag` controller so the dependency survives any future split of the role. `make generate-manifests` regenerates `config/rbac/role.yaml`.

DevOps-user-facing RBAC is deliberately **not** extended: no role in `config/` grants `Tag` access to namespace users, and admission rule V6 backs that up.

---

## Conversion strategy

None. `v1alpha1` is the only version of the type, so there is no conversion webhook and no `utilconversion.MarshalData` annotation round-trip to worry about. If a future version is added, the storage version marker (`+kubebuilder:storageversion:true`) already sits on `v1alpha1` and a hub/spoke conversion would be introduced then, following the pattern in `api/`.

Because the `Tag` resource is internal bookkeeping (spec NG7), it carries no compatibility obligation toward DevOps users; the compatibility obligation is toward the vSphere policy module's other consumers, which is why the type lives in that module rather than in `api/` (D1).

---

## Canonical examples per user story

### US1 — VM references its own label

VM:

```yaml
apiVersion: vmoperator.vmware.com/v1alpha6
kind: VirtualMachine
metadata:
  namespace: ns-1
  name: vm-a
  labels:
    app: nginx
spec:
  affinity:
    vmAffinity:
      requiredDuringSchedulingPreferredDuringExecution:
        - labelSelector:
            matchLabels:
              app: nginx
          topologyKey: kubernetes.io/hostname
```

Resulting `Tag` (`ns-1/tag-<hash>`): `spec.label: app`, `spec.value: nginx`, `metadata.labels: {app: nginx}`, one owner reference to `vm-a`. Resulting vCenter tag on `vm-a`: name `app:nginx`, category `ns-1`.

### US2 — label-only VM

```yaml
apiVersion: vmoperator.vmware.com/v1alpha6
kind: VirtualMachine
metadata:
  namespace: ns-1
  name: vm-label-only
  labels:
    app: nginx
spec: {}   # no affinity
```

The `Tag` above is **unchanged** — `vm-label-only` does not become an owner — but `vm-label-only` receives vCenter tag `app:nginx` in category `ns-1` on its next reconcile, triggered by the `Tag` controller's fan-out.

### US3 — affinity changed away from the label

**Case 1 — another VM still owns the `Tag`.** `vm-a`'s `spec.affinity` stops matching `app=nginx` while `vm-b` still references it:

- `vm-a`'s owner reference is removed; `vm-b`'s remains, so the `Tag` survives.
- `vm-a` still carries `app: nginx`, so it **keeps** vCenter tag `app:nginx` — it is now a label-only participant, identical in every observable way to `vm-label-only` above.
- No `TagSpec` is emitted for `vm-a` at all: the desired set is unchanged.

**Case 2 — `vm-a` was the last owner.** `vm-a`'s `spec.affinity` stops matching `app=nginx` and no other VM references it:

- `vm-a`'s owner reference is removed → `ownerReferences` is empty.
- The `Tag` controller observes the empty list, enqueues `vm-a` and `vm-label-only`, and deletes the `Tag`.
- Both VMs' next reconcile emits `TagSpec{Operation: remove}` for `app:nginx`.
- The `Tag` controller releases the finalizer; the resource disappears.
