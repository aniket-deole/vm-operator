# Feature Specification: Tag CRD + Tag Controller for Affinity

- **Feature branch**: [`aniketd/tag-controller-for-affinity`](https://github.com/aniket-deole/vm-operator/tree/aniketd/tag-controller-for-affinity)
  - **Fork**: `aniket-deole/vm-operator`
  - **PR target**: `vmware-tanzu/vm-operator`
- **Created**: 2026-08-04
- **Status**: Draft
- **Epic**: vmop-3882

---

## Summary

The Kubernetes platform lets users schedule workloads with affinity or anti-affinity to other workloads using label selectors, including affinity against all current _and_ future workloads matching a set of criteria. VM Service delivered the equivalent for VM-based workloads in VCF 9.1, but only for members of a `VirtualMachineGroup`, and only at create time — execution-time affinity/anti-affinity rules on existing workloads could not be altered.

VMs participate in affinity/anti-affinity placement rules by carrying Kubernetes labels and by referencing those labels from `spec.affinity`. When a workload is reconciled:

- Labels participating in placement are converted to vCenter Tags.
- `spec.affinity` policies are converted into vCenter Compute Policies.

vCenter manages the lifecycle of the Tags and Compute Policies used for placement.

This feature:

- Introduces a Supervisor `Tag` API that records, per namespace, which label key/value pairs currently participate in an affinity relationship and which VMs own that participation.
- Uses that record to tag **every** VM in the namespace carrying a participating label — including VMs that carry the label but declare no `spec.affinity` of their own — so that both initial and execution-time placement can be satisfied.
- Relaxes the current restriction that `spec.affinity` is immutable, so affinity relationships can be changed on a running VM.

The `Tag` API is **invisible to DevOps users**: they continue to express intent solely through VM labels and `spec.affinity`, and are not permitted to create, modify, or delete `Tag` resources. The `Tag` resource is an internal bookkeeping surface for VM Operator, visible to a CSP admin or platform engineer for diagnosis.

---

## Goals

- **G1**: A VM whose `spec.affinity` references one of its own labels **MUST** have the corresponding vCenter Tag applied, and a `Tag` resource for that label key/value **MUST** exist in the VM's namespace with that VM recorded as an owner.
- **G2**: Every VM in the namespace carrying a label that some VM's `spec.affinity` references **MUST** be tagged with the corresponding vCenter Tag, regardless of what that VM's own `spec.affinity` says (including declaring none at all), and regardless of the order in which the VMs were created. See "Ownership vs. tag carriage" below.
- **G3**: A label that no VM's `spec.affinity` references **MUST NOT** produce a `Tag` resource and **MUST NOT** produce a vCenter Tag on any VM.
- **G4**: A `Tag` resource **MUST** persist for exactly as long as at least one VM owns it — that is, carries the label and references it from `spec.affinity` — and **MUST** be removed once ownership reaches zero, whether ownership drops through VM deletion or through an affinity change.
- **G5**: When a `Tag` resource is removed, the vCenter Tag **MUST** be removed from every VM in that namespace that carried it, including label-only VMs.
- **G6**: Tag participation **MUST** be scoped to a single namespace. The same label key/value in two namespaces **MUST** produce two distinct `Tag` resources, and a VM **MUST NOT** be tagged on account of a `Tag` resource in another namespace.
- **G7**: `spec.affinity` **MUST** become mutable when this feature is enabled, and a change to it **MUST** converge both the VM's own tags and the tags of every other VM in the namespace affected by the change.
- **G8**: Tag application and removal **MUST** be independent of `VirtualMachineGroup` membership.
- **G9**: DevOps users **MUST NOT** be able to create, update, or delete a `Tag` resource; only privileged accounts may. `spec.label` and `spec.value` **MUST** be immutable after create.
- **G10**: A CSP admin listing `Tag` resources **MUST** be able to see, from the default `kubectl get` output, which label key and value each `Tag` represents.
- **G11**: The entire behavior **MUST** be gated behind a feature flag, and with the flag off, tagging behavior **MUST** be exactly what it is today.
- **G12**: A `Tag` resource pending deletion that is concurrently needed again **MUST** be re-created; the request **MUST** be retried until it succeeds rather than silently dropped.

## Non-goals

- **NG1**: No vCenter-side work is performed on behalf of a `Tag` resource. The `Tag` resource is bookkeeping only; the vCenter Tag itself is created by `vpxd` when named in a VM's reconfigure request, or is assumed to pre-exist. The `Tag` resource's reserved identity field for a resolved vCenter Tag UUID is **not** populated by this feature.
- **NG2**: Multi-vCenter. A `Tag` records a target vCenter identifier for forward-compatibility only; a single vCenter is assumed.
- **NG3**: Removing the `VirtualMachineGroup` dependency from affinity as a whole. This feature is the first incremental step; the group-based path continues to exist.
- **NG4**: Retiring the pre-existing create-time label-to-tag mechanism. That mechanism continues to serve the flag-off path in this feature; its removal is deferred to a follow-up spec.
- **NG5**: Compute Policy generation. The vCenter Compute Policies that consume these tags are generated by the existing affinity path and are unchanged by this feature.
- **NG6**: Tying the feature flag to a Supervisor capability. The flag is introduced standalone and is enabled by default on the development branch; capability wiring is deferred.
- **NG7**: Exposing the `Tag` API to DevOps users, or documenting it as a user-facing API.
- **NG8**: Support for label selector operators beyond what the existing affinity path supports (`matchLabels`, and `matchExpressions` with the `In` operator).

---

## Key entities

### Tag

A namespace-scoped resource representing a single vCenter Tag, corresponding to exactly one label **key + value** pair (e.g. `app=nginx`). A distinct key+value pair yields a distinct `Tag`. Its lifecycle is driven by how many VMs currently participate in the associated label/affinity relationship.

vCenter Tags are identified by name **and** category, and are unique across that combination. Every vCenter Tag backing this feature uses the **namespace name** as its Tag Category, derived from the `Tag`'s own namespace — the category is not a separate user-settable field.

Attributes:

- The label **key** and label **value** the `Tag` represents, both required, and both also mirrored into the `Tag`'s own metadata labels, so an admin can select `Tag`s by label. (The control plane answers "does a `Tag` already exist for this key/value?" from the resource's derived name, not from that mirror — see `model.md` "Query surface".)
- The target vCenter identifier, for forward-compatibility (see NG2).
- A reserved identity field for the resolved vCenter Tag UUID, left empty by this feature (see NG1).
- The set of owning VMs, recorded as owner references. An owner is a VM that both carries the label **and** references it from its own `spec.affinity`. A VM that merely carries the label is **not** an owner, even though it does get the vCenter Tag.

### VirtualMachine

The workload. The attributes relevant here are its labels and its `spec.affinity` rules. A VM becomes an owner of zero or more `Tag` resources and carries the corresponding vCenter Tags.

### vCenter Tag

The vCenter-side object (name + category) attached to a VM and consumed by Compute Policies for placement. It is created by `vpxd` when the VM's reconfigure request names it, or is assumed to pre-exist. It is not lifecycle-managed by VM Operator (NG1).

### Compute Policy

The vCenter mechanism that enforces affinity/anti-affinity using vCenter Tags. It consumes the tags this feature applies and is otherwise unchanged (NG5).

### Ownership vs. tag carriage

Two distinct rules govern this feature, and conflating them is the single easiest way to misread it:

1. **Ownership** — which VMs own a `Tag`, and therefore whether it exists at all:

   > A VM owns the `Tag` for a label pair **iff** it carries that label **and** its own `spec.affinity` references it.

2. **Tag carriage** — which VMs carry the vCenter Tag:

   > A VM carries the vCenter Tag for a label pair **iff** it carries that label **and** a `Tag` for that pair exists in its namespace.

Rule 2 makes no reference to the VM's own `spec.affinity`. Consequences worth stating explicitly, because each one is a scenario below:

- A VM carrying the label with no `spec.affinity` at all is tagged, but is not an owner (US2).
- A VM that stops referencing a label it still carries **loses ownership but keeps the vCenter Tag**, for as long as some other VM still owns the `Tag` (US3 scenario 2). It becomes indistinguishable from any other label-only VM, which is exactly right: `spec.affinity` expresses which relationships a VM *asks for*, while its labels express which relationships it *is available for*.
- A VM only loses the vCenter Tag when it drops the label, or when the `Tag` itself is deleted because ownership reached zero (US3 scenarios 1 and 3).
- A label pair referenced by a VM that does not carry it produces no `Tag`, because ownership requires both (US1 scenario 3).

### Namespace isolation

Interaction between `Tag` resources and workloads is confined to a single namespace. Workloads and `Tag` resources never reference objects across namespaces. Because the vCenter Tag Category is the namespace name, the same label value in two namespaces yields two distinct vCenter Tags and two distinct `Tag` resources (G6).

---

## Relationship to the existing tagging mechanisms

Two tag-configuring mechanisms exist today:

1. A UUID-based path driven by the vSphere Policy reconciler, which consumes tag UUIDs from evaluated policies.
2. A create-time path that converts a VM's own affinity-referenced labels directly into tag specifications on the VM's reconfigure request.

This feature **supersedes** mechanism (2) functionally and **coexists** with mechanism (1). Concretely: when the feature flag is on, the `Tag`-driven path is what decides which vCenter Tags a VM carries; when the flag is off, mechanism (2) behaves exactly as it does today (G11, NG4).

Mechanism (2) is limited to labels referenced by the VM's *own* `spec.affinity`, and only at create time. The `Tag`-driven path removes both limits: participation is namespace-wide (G2) and is re-evaluated on every reconcile (G7).

---

## User scenarios & testing *(mandatory)*

### User Story 1 — Label referenced by a new VM's affinity becomes a vCenter Tag (Priority: P0)

A DevOps user creates a VM that carries a label and references that label in `spec.affinity`. A matching vCenter Tag is applied to the VM and a `Tag` resource exists in the VM's namespace, with the VM recorded as an owner.

**Why this priority**: This is the core capability — without translating affinity-referenced labels into vCenter Tags, no placement policy can be enforced. It delivers a demonstrable MVP on its own.

**Independent test**: Create a single VM with label `LBL-1` and `spec.affinity` matching `LBL-1`, reconcile it, and verify the vCenter Tag is applied, the `Tag` resource is created in the namespace, and the VM is listed as an owner.

**Acceptance scenarios**:

1. **Given** VM-A with label `LBL-1` and `spec.affinity` matching `LBL-1`, **When** VM-A is reconciled, **Then** vCenter Tag `LBL-1` is applied to VM-A, a `Tag` resource for `LBL-1` is created in VM-A's namespace, and VM-A is added as an owner of that `Tag`.
2. **Given** VM-A with label `LBL-1` and `spec.affinity` matching `LBL-1` and an existing `Tag` for `LBL-1` owned by VM-A, **When** VM-B with label `LBL-1` and `spec.affinity` matching `LBL-1` is created and reconciled, **Then** vCenter Tag `LBL-1` is applied to VM-B, VM-B is added as an owner of the `Tag`, and the `Tag` has two owners.
3. **Given** VM-A with labels `LBL-1` and `LBL-2` and `spec.affinity` matching only `LBL-2`, **When** VM-A is reconciled on create, **Then** vCenter Tag `LBL-2` (only) is applied to VM-A, a `Tag` for `LBL-2` is created in the namespace, and it records VM-A as an owner. No `Tag` is created for `LBL-1`.
4. **Given** VM-A and VM-B both with label `LBL-1` and `spec.affinity` matching `LBL-1`, **When** VM-A is deleted, **Then** VM-A is removed as an owner of the `Tag`, the `Tag` is **not** deleted, and VM-B keeps the vCenter Tag.
5. **Given** VM-A is the only owner of the `Tag` for `LBL-1`, **When** VM-A is deleted, **Then** the `Tag` is deleted from the namespace.

---

### User Story 2 — Existing labeled VM is tagged when a later VM references that label (Priority: P1)

A VM already exists carrying a label but with no `spec.affinity`. When another VM is later created with that same label and a `spec.affinity` that references it, the pre-existing VM is proactively reconciled so that it, too, receives the vCenter Tag and participates in the affinity relationship. Removing the referencing VM reverses the tagging on the pre-existing VM.

**Why this priority**: A VM must be able to establish affinity with VMs that already exist. Without proactive reconciliation of pre-existing labeled VMs, affinity policies would silently miss participants.

**Independent test**: Create VM-A with label `LBL-1` and no affinity; confirm it is untagged. Then create VM-B with label `LBL-1` and `spec.affinity` matching `LBL-1`; verify VM-A is proactively tagged and a `Tag` resource is created.

**Acceptance scenarios**:

1. **Given** VM-A exists with label `LBL-1` but no `spec.affinity`, **When** VM-B is created with label `LBL-1` and `spec.affinity` matching `LBL-1`, **Then** vCenter Tag `LBL-1` is applied to both VM-A and VM-B, and a `Tag` for `LBL-1` is created in the namespace with VM-B — and only VM-B — as an owner.
2. **Given** VM-A exists with label `LBL-1` and no `spec.affinity`, and VM-B exists in the same namespace with label `LBL-1` and `spec.affinity` matching `LBL-1`, **When** VM-B is deleted, **Then** vCenter Tag `LBL-1` is removed from VM-A, and the `Tag` is deleted because no VM owns it any longer.
3. **Given** VM-A exists with label `LBL-1` and no `spec.affinity`, **When** VM-B is created in a **different** namespace with label `LBL-1` and `spec.affinity` matching `LBL-1`, **Then** vCenter Tag `LBL-1` is applied to VM-B and a `Tag` is created in VM-B's namespace, and vCenter Tag `LBL-1` is **not** applied to VM-A (namespace isolation).
4. **Given** a `Tag` for `LBL-1` exists in the namespace, **When** a new VM-C carrying label `LBL-1` and no `spec.affinity` is created, **Then** vCenter Tag `LBL-1` is applied to VM-C on its first reconcile, and VM-C is **not** added as an owner of the `Tag`.

---

### User Story 3 — VM's affinity spec is changed and tagging converges (Priority: P1)

A DevOps user changes `spec.affinity` on an existing VM. Tag ownership and vCenter Tags converge across the namespace, and a `Tag` resource is removed only when no VM owns it any longer.

**Why this priority**: This is an enhancement over US1 and US2 and relaxes the current limitation that `spec.affinity` is immutable.

**Independent test**: Create a single VM with label `LBL-1` and `spec.affinity` matching `LBL-1` and verify the vCenter Tag, the `Tag` resource, and the ownership. Then mutate `spec.affinity` and verify tag and `Tag` cleanup.

**Acceptance scenarios**:

1. **Given** VM-A with label `LBL-1` and `spec.affinity` matching `LBL-1`, **When** `spec.affinity` is changed so it no longer matches `LBL-1`, **Then** vCenter Tag `LBL-1` is removed from VM-A, VM-A is removed as an owner, and the `Tag` is deleted because no owners remain.
2. **Given** VM-A and VM-B both with label `LBL-1` and `spec.affinity` matching `LBL-1`, **When** VM-A's `spec.affinity` is changed to no longer match `LBL-1`, **Then** VM-A is removed as an owner, the `Tag` is **not** deleted because VM-B still owns it, and VM-A **keeps** vCenter Tag `LBL-1` because it still carries the label — VM-A is now a label-only participant, indistinguishable from VM-A in US2 scenario 1 (see "Ownership vs. tag carriage").
3. **Given** VM-A exists with label `LBL-1` and no `spec.affinity`, and VM-B exists in the same namespace with label `LBL-1` and `spec.affinity` referencing `LBL-1`, **When** `LBL-1` is removed from VM-B's `spec.affinity` and no other VM references `LBL-1`, **Then** vCenter Tag `LBL-1` is removed from both VM-A and VM-B, and the `Tag` is deleted in that namespace.
4. **Given** VM-A with labels `LBL-1` and `LBL-2` and `spec.affinity` matching only `LBL-1`, **When** `spec.affinity` is changed to match `LBL-2` instead, **Then** a `Tag` for `LBL-2` is created owned by VM-A and vCenter Tag `LBL-2` is applied to VM-A; VM-A is removed as an owner of the `Tag` for `LBL-1`; and vCenter Tag `LBL-1` is removed from VM-A **only if** that `Tag` is deleted for want of owners — if another VM still owns it, VM-A keeps `LBL-1` as a label-only participant.
5. **Given** VM-A with labels `LBL-1` and `LBL-2` and `spec.affinity` matching only `LBL-1`, **When** both the `LBL-1` label and its `spec.affinity` reference are removed from VM-A, and `spec.affinity` matches `LBL-2` instead, **Then** VM-A ends up carrying vCenter Tag `LBL-2` only — it no longer carries `LBL-1` at all, so it cannot be a label-only participant in it.
6. **Given** the feature is disabled, **When** a DevOps user attempts to change `spec.affinity`, **Then** the request is rejected exactly as it is today.

---

### User Story 4 — CSP admin diagnoses affinity tagging (Priority: P2)

A CSP admin inspects which label relationships are currently driving affinity in a namespace, and which VMs are responsible for each.

**Why this priority**: Diagnosability. Without it, an admin explaining why a VM carries a given vCenter Tag has to reconstruct the relationship from every VM's `spec.affinity` by hand.

**Independent test**: With two VMs participating in two different label relationships in a namespace, listing `Tag` resources shows both, each identifying its label key and value; and the owner references on each `Tag` identify the responsible VMs.

**Acceptance scenarios**:

1. **Given** `Tag` resources exist in a namespace, **When** a CSP admin lists them, **Then** the default output identifies, for each `Tag`, the label key and the label value it represents.
2. **Given** a `Tag` exists, **When** a CSP admin inspects it, **Then** its owner references name every VM that carries the label and references it from `spec.affinity`, and its `Ready` condition and last-observed generation reflect the current state.
3. **Given** a DevOps user (non-privileged account), **When** they attempt to create, modify, or delete a `Tag`, **Then** the request is rejected by admission.
4. **Given** a privileged account, **When** they attempt to change a `Tag`'s label key or value after create, **Then** the request is rejected as immutable.

---

## Edge cases

- **Tag deletion racing a re-create**: When a `Tag` is marked for deletion and, concurrently, a VM is reconciled that needs the same `Tag`, the create MUST be retried until it succeeds. The reconcile MUST NOT proceed as though the `Tag` existed, and MUST NOT silently skip tagging the VM (G12).
- **Ownership dropping to zero while the owning VM still exists**: When the last owner stops referencing the label but is not itself deleted, the `Tag` must still be removed (G4). This case is not covered by ordinary owner-reference garbage collection, which acts only when owners are *deleted*; it requires an explicit delete.
- **Concurrent owners writing the same `Tag`**: Several VMs in a namespace add and remove their own owner reference on the same `Tag`. The owner-reference list must not lose a concurrent writer's entry.
- **A VM stops referencing a label it still carries**: it loses ownership but keeps the vCenter Tag while any other VM still owns the `Tag`. Tag carriage is decided by the VM's labels and the `Tag`'s existence, never by the VM's own `spec.affinity` (see "Ownership vs. tag carriage"). A level-triggered reconciler cannot distinguish "never referenced this label" from "stopped referencing it" without keeping history, and it must not: both are the same state and must behave identically.
- **A VM references a label it does not carry**: no `Tag` is created for it, because ownership requires carrying the label as well. The referencing VM's affinity toward that pair is only enforceable once some VM both carries and references it.
- **A VM carries a label but the `Tag`'s key/value match is only partial**: A `Tag` represents a key **and** a value. A VM carrying the same key with a different value is unrelated to that `Tag` and is not tagged from it.
- **Empty label value**: A label with an empty value is a legal Kubernetes label and yields a `Tag` distinct from any non-empty value for the same key.
- **A label key or value that cannot appear verbatim in a resource name**: Label keys may be prefixed (`example.com/tier`) and values may be long. The `Tag`'s resource name is therefore derived rather than composed literally, so it is always a valid resource name (see `model.md`).
- **Unsupported label selector operators**: Selector expressions the existing affinity path does not support are ignored for tag derivation exactly as they are today (NG8) — they do not fail the VM's reconcile.
- **A VM that carries a participating label and is a VKS node VM, or has an explicit zone**: Whether host-topology and zone-topology affinity terms are considered at all continues to follow the existing constraint rules; this feature does not change which terms are eligible, only what is done with the labels those terms reference.
- **Flag turned off after tags were applied**: With the flag off, the `Tag`-driven path stops running. Already-applied vCenter Tags are not proactively cleaned up, and existing `Tag` resources are left in place; the pre-existing create-time mechanism resumes deciding tags at create time.

---

## Success criteria *(mandatory)*

### Measurable outcomes

- **SC-001**: A VM created with a label referenced by its `spec.affinity` has the corresponding vCenter Tag applied and a `Tag` resource present with the VM as owner, with no DevOps user action beyond setting the label and the affinity.
- **SC-002**: VMs sharing an affinity relationship in a namespace carry the matching vCenter Tag regardless of creation order — a pre-existing labeled VM is tagged once a later VM references the label, and is untagged once the last referencing VM stops referencing it.
- **SC-003**: A `Tag` resource persists for exactly as long as at least one VM owns it and is removed once ownership reaches zero — verifiable across add / remove / affinity-change / VM-delete sequences with no orphaned `Tag` resources and no orphaned vCenter Tags.
- **SC-004**: The tagging mechanism applies vCenter Tags to participating VMs based solely on their labels and `spec.affinity`, independent of any `VirtualMachineGroup` membership. A consequence is that VMs in different `VirtualMachineGroup`s sharing the same affinity label share affinity via the same vCenter Tag — more efficiently and more broadly (including label-only VMs) than the pre-existing create-time behavior. This is intentional, and is the first incremental step toward removing the `VirtualMachineGroup` dependency.
- **SC-005**: `spec.affinity` can be changed on an existing VM when the feature is enabled, and the resulting tag changes converge without a VM restart or re-create.
- **SC-006**: With the feature flag off, tagging and `spec.affinity` mutability behave exactly as they do today, verifiable by the pre-existing test suites passing unchanged.

## Open questions

None outstanding. Decisions previously open and now settled — the API's group and version, the division of labor between the VM reconcile path and the `Tag` controller, the feature-flag strategy, the fate of the pre-existing create-time mechanism, resource-name derivation, the vCenter tag name format, `spec.affinity` mutability, admission validation, the split between ownership and tag carriage, and how each `Tag` query is served — are recorded in [`research.md`](./research.md) "Decision log".

## Review & acceptance checklist

- [x] All user stories have at least two Given/When/Then scenarios.
- [x] Each scenario is independently testable.
- [x] Out-of-scope items are listed (see "Non-goals").
- [x] Namespace isolation is specified.
- [x] `Tag` lifecycle at zero owners is specified for both the VM-deleted and the affinity-changed paths.
- [x] Ownership and tag carriage are specified as separate rules, and every scenario is consistent with both.
- [x] Feature-flag-off behavior is specified.
- [x] DevOps-user invisibility (admission) is specified.
