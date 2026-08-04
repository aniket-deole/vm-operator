# Research: Tag CRD + Tag Controller for Affinity

- **Spec**: [`spec.md`](./spec.md)
- **Epic**: vmop-3882
- **Date**: 2026-08-04

This is the investigation log behind [`spec.md`](./spec.md), [`plan.md`](./plan.md), and [`model.md`](./model.md). It records the prior art in this repository, the platform semantics the design depends on, and the decisions taken (with the alternatives rejected).

---

## Prior art in this repository

### Mechanism A — create-time label→tag conversion (`affinity.go`)

`pkg/providers/vsphere/virtualmachine/affinity.go` holds today's label-to-tag conversion:

- `extractAffinityLabelsFromVM(vmCtx, constraints) sets.Set[string]` walks `vm.Spec.Affinity.VMAffinity` and `.VMAntiAffinity`, both the `RequiredDuringSchedulingPreferredDuringExecution` and `PreferredDuringSchedulingPreferredDuringExecution` term lists, and returns the referenced labels as `"key:value"` strings. Terms are skipped when their `TopologyKey` is not eligible under the passed `AffinityRuleConstraints`.
- `genConfigSpecTagSpecsFromVMLabels(vmCtx, configSpec, constraints)` intersects the VM's **own** labels (after `kubeutil.RemoveVMOperatorLabels`) with that set, and appends one `vimtypes.TagSpec` per survivor with `Operation: add` and `Id.NameId = {Tag: "<key>:<value>", Category: vm.Namespace}`.
- `extractLabelsFromSelector` supports `matchLabels` and `matchExpressions` with the `In` operator only; any other operator returns an error which callers log at V(4) and skip. Labels are emitted in `"key:value"` form and sorted.

Call sites are both in `pkg/providers/vsphere/virtualmachine/configspec.go`: once in `CreateConfigSpec` (line ~158) and once in `CreateConfigSpecForPlacement` (line ~318). `CalculateAffinityConstraints(vmCtx, isCreateVM)` (line ~351) derives the constraints from `Features.VMPlacementPolicies` (zone rules), `Features.VMAffinityDuringExecution` (host rules), CAPI/VKS labels, and the presence of a zone label.

Limits that motivate this feature, both acknowledged in the existing code comments (including a `TODO(for Day 2 operations)`):

1. Only labels referenced by the VM's **own** `spec.affinity` become tags — a VM that carries the label but declares no affinity is never tagged, so it silently misses the relationship.
2. It only runs on the create/placement config-spec paths, so an affinity change on an existing VM does not converge tags.

Both call sites also emit `genConfigSpecAffinityPolicies`, which builds the `VmPlacementPolicies` (`VmVmAffinity`, `VmVmAntiAffinity`, `VmToVmGroupsAntiAffinity`) that *consume* these tags. That part is out of scope here (spec NG5).

### Mechanism B — UUID-based tagging via the vSphere Policy reconciler

`pkg/vmconfig/policy/` (`policy_reconciler.go`, `policy_reconciler_util.go`) creates a `vspherepolv1.PolicyEvaluation` per VM (named `vm-<vmName>`) via `controllerutil.CreateOrPatch`, feeding it image name/labels, guest ID/family, workload labels, and explicitly-referenced policies, then consumes the results. `vspherepolv1.TagPolicySpec.Tags` carries **UUIDs** of vSphere tags to apply. This is the UUID-based path referenced in the source spec.

This feature coexists with mechanism B: it neither reads nor writes `PolicyEvaluation`/`TagPolicy`, and the two produce disjoint `TagSpec` entries (UUID-identified vs. name+category-identified).

### The `vmconfig.Reconciler` extension point

`pkg/vmconfig/vmconfig_reconciler.go` defines:

```go
type Reconciler interface {
    Name() string
    Reconcile(ctx, k8sClient ctrlclient.Client, vimClient *vim25.Client,
        vm *vmopv1.VirtualMachine, moVM mo.VirtualMachine,
        configSpec *vimtypes.VirtualMachineConfigSpec) error
    OnResult(ctx, vm, moVM, resultErr error) error
}
```

This is the only extension point in the VM reconcile path that has **both** a Kubernetes client and the outgoing `ConfigSpec` in hand — exactly what the Tag-driven path needs (read/write `Tag` resources, then emit `TagSpecs`). Existing implementations: `crypto`, `diskpromo`, `bootoptions`, `extraconfig`, `networkextraconfig`, `policy`, `cdrom`, `volumes`, `virtualcontroller`, `anno2extraconfig`.

Registration is per-reconcile and feature-gated in `controllers/virtualmachine/virtualmachine/virtualmachine_controller.go` `Reconcile` (~line 340):

```go
ctx = vmconfig.WithContext(ctx)
if pkgcfg.FromContext(ctx).Features.BringYourOwnEncryptionKey {
    ctx = vmconfig.Register(ctx, crypto.New())
}
...
ctx = vmconfig.Register(ctx, bootoptions.New())
```

`moVM` gives the reconciler the VM's live state; the VM update flow already fetches attached tags as step 3 of the documented reconcile order in `operator-best-practices.md` ("VM Update Reconcile Order"), which is what makes an add/remove **diff** possible rather than blind adds.

### The `cource` channel source

`pkg/util/kube/cource` is the repository's documented mechanism for enqueuing a reconcile from outside the watch pipeline. The VM controller already consumes it (`virtualmachine_controller.go` ~line 127):

```go
builder = builder.WatchesRawSource(source.Channel(
    cource.FromContextWithBuffer(ctx, "VirtualMachine", 100),
    &handler.EnqueueRequestForObject{}))
```

gated on `pkgcfg.FromContext(ctx).AsyncSignalEnabled` (default `true`, `pkg/config/default.go`). This is how the `Tag` controller fans out to VMs — including label-only VMs — without the VM controller needing a watch on `Tag`, and without a cross-controller queue dependency.

### `vsphere.policy.vmware.com` API and its manifests

`external/vsphere-policy/` is its own Go module (`github.com/vmware-tanzu/vm-operator/external/vsphere-policy`), aliased `vspherepolv1`, group `vsphere.policy.vmware.com`, version `v1alpha1`. Existing types: `ComputePolicy`, `TagPolicy`, `PolicyEvaluation`, plus shared matcher types in `common_types.go`. Each type appends itself to `objectTypes` in an `init()`; `groupversion_info.go` wires `AddToScheme`.

CRD manifests for this module are generated into `config/crd/external-crds/` by `make generate-external-manifests`, which already runs `controller-gen` over `paths=github.com/vmware-tanzu/vm-operator/external/vsphere-policy/...`. The existing outputs are `vsphere.policy.vmware.com_computepolicies.yaml`, `_policyevaluations.yaml`, and `_tagpolicies.yaml` — so a new type in the module is picked up with no Makefile change.

`config/crd/external-crds/README.md` and `test/builder/fake.go` (`KnownObjectTypes`) are the two places that must learn about a new external type for envtest and fake-client tests respectively.

Controllers for this group live under `controllers/vspherepolicy/` (currently only `policyevaluation/`), registered from `controllers/vspherepolicy/controllers.go`, which is itself called from `controllers/controllers.go` (~line 91). This matches the constitution's rule that controllers for non-`vmoperator.vmware.com` groups are not placed directly in `controllers/`.

### `spec.affinity` immutability

`webhooks/virtualmachine/validation/virtualmachine_validator.go`:

- `validateVMAffinity` (~line 3262) validates the shape on create and already branches on `Features.VMAffinityDuringExecution` for host-topology terms.
- `validateImmutableVMAffinity` (~line 3463) is called from the update path (~line 2497) and forbids **any** change to `spec.affinity`, with **no** feature gate:

```go
if !equality.Semantic.DeepEqual(vm.Spec.Affinity, oldVM.Spec.Affinity) {
    allErrs = append(allErrs, field.Forbidden(p, "updating Affinity is not allowed"))
}
```

US3 is therefore blocked on relaxing this check. The decision (D7 below) is to gate the relaxation on the new flag so flag-off Supervisors keep today's behavior (spec G11, SC-006).

### Feature flags

`pkg/config/config.go` `FeatureStates` (~line 190-225) holds the flag set; `pkg/config/capabilities/capabilities.go` maps Supervisor capability keys onto those fields (e.g. `CapabilityKeyVMAffinityDuringExecution = "supports_VM_service_VM_affinity_during_execution"`); `pkg/config/default.go` `Default()` seeds the handful of flags that default to `true`.

### Name-hashing helper

`pkg/util/hash.go` provides `SHA1Sum17(s string) string` — the first 17 hex characters of a SHA-1 sum — and the `VMIName` precedent (`"vmi-" + SHA1Sum17(...)`) for deriving a DNS-safe resource name from an arbitrary identifier. This is reused for the `Tag` resource name (D5).

### E2E prior art

Affinity/anti-affinity E2E coverage lives in `test/e2e/vmservice/vmservice/virtualmachine/vm_group.go` (`createVMWithAffinityAndAntiAffinityFunc`, `createHostVMWithAffinityAndAntiAffinityFunc`), which builds VMs with affinity terms and asserts placement via `wcp.ComputePolicyCapabilityVMHostAffinity`. `test/e2e/vmservice/consts/consts.go` already defines `VMAffinityDuringExecutionCapabilityName`. `test/e2e/infrastructure/vsphere/wcp/iaas_policies.go` holds the vSphere-side policy helpers. New coverage for this feature is specified in [`e2e.md`](./e2e.md).

---

## Platform semantics the design depends on

### Owner-reference garbage collection does **not** cover "ownership dropped to zero"

Kubernetes garbage collection deletes a dependent when the owners named in its `ownerReferences` are **deleted**. It does not delete an object merely because its `ownerReferences` list became empty while those owners still exist — such an object is treated as an orphan and is retained indefinitely.

Consequence: the source spec's note that an empty owner-reference list means "GC deletes the `Tag`" is only true for the VM-deletion path (spec US1 scenario 5, US2 scenario 2). The affinity-change path (spec US3 scenarios 1 and 3), where the VM stops referencing the label but still exists, needs an **explicit delete**. The design assigns that delete to the `Tag` controller, which already watches `Tag` and can observe the empty list; ordinary GC remains as the backstop for the all-owners-deleted case. This is recorded as an edge case in `spec.md` and specified in `plan.md`.

### `TagSpec` supports removal, so tags can be diffed

`vimtypes.TagSpec` embeds `ArrayUpdateSpec`, so `ArrayUpdateOperationRemove` is available alongside `add`. Combined with the attached-tag list the update flow already fetches, this makes the tag set a diff (add missing, remove extra) rather than an append-only operation — required by spec G5 and US3.

### vCenter Tag identity is name + category

vCenter Tags are unique per (name, category). This feature keeps today's convention: name `"<key>:<value>"`, category `<namespace>` (D6), which is what makes namespace isolation (spec G6) fall out for free and keeps the emitted `TagSpec`s interchangeable with the Compute Policy side, which builds `TagId.NameId` the same way in `buildTagIDsFromTopology`.

### Fan-in writes to a shared list field need an optimistic lock

Several VMs in a namespace add and remove their own entry in the same `Tag`'s `ownerReferences` — a fan-in write to a **list**-typed field. Per `operator-best-practices.md` ("Fan-out to Child Objects") and the constitution, a plain merge patch replaces list fields wholesale with no conflict detection and can silently drop a concurrent writer's entry, so these writes use `client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})`, compare with `apiequality.Semantic.DeepEqual`, and skip when nothing changed. `controllerutil.CreateOrPatch` is therefore **not** appropriate for the ownership write, despite being the repository default for fan-out.

### `client.MatchingLabels` is not an indexed lookup

controller-runtime's cache maintains **field** indexes only — those registered via `mgr.GetFieldIndexer().IndexField(...)`. A label selector passed to a cached client is applied as an **in-memory filter after** the cache has returned every object in the namespace; there is no label index to consult. (The API server behaves the same way for arbitrary labels on a direct, uncached `List`.)

Three consequences for this design:

1. The `Tag`'s `metadata.labels` mirror does **not** make the control plane's queries efficient. It is a convenience surface for `kubectl get tags -l app=nginx` and for legibility of a hash-named resource — worth keeping for those reasons, but it must not be the mechanism behind "does a `Tag` for this label exist?" on the reconcile hot path. That question is answered by a `Get` on the derived name instead, which is a point read.
2. Every other repeated `Tag` query gets an explicit field index: `spec.label` (all `Tag`s for a key), `spec.labelKeyValue` (exact pair when the name has not been derived), and `metadata.ownerReferences.uid` (which `Tag`s a VM owns). See `plan.md` "Field indexes and query patterns" and `model.md` "Query surface".

3. The `Tag` controller's fan-out — "which VMs carry this label?", the query spec US2 depends on — gets a field index too: `metadata.labels.keyValue` on `vmopv1.VirtualMachine`, whose extractor emits one `"<key>:<value>"` entry per label after `kubeutil.RemoveVMOperatorLabels`. The `List` then returns only the VMs carrying the label, so fan-out cost scales with the size of the affinity relationship rather than with the namespace's VM count.

A multi-valued extractor over label pairs is idiomatic in this repository, not exotic: `pkg/util/kube/pvc.go`'s `VMSpecVolumesPVCsIndexerFunc` already emits one entry per PVC a VM references, and `RemoveVMOperatorLabels` returns a fresh map (`pkg/util/kube/label.go`) so calling it from an extractor cannot mutate the cached object. Filtering the reserved `vmoperator.vmware.com`-domain labels out also keeps the index free of entries that could never match a `Tag`, since the same filter governs which labels can participate in affinity today.

Two claims in earlier drafts of this document were wrong and are retracted here:

- "The informer's label index serves this directly and no `IndexField` registration is needed" — there is no label index; see above.
- "A field index cannot be built over arbitrary label keys ... so the fan-out keeps an in-memory filter" — conflating *controller-runtime not maintaining a label index by default* with *a per-pair index being infeasible*. A custom multi-valued extractor supplies exactly that index at a cost proportional to the label maps already in cache, which is why the fan-out is indexed after all.

---

## Decision log

Decision IDs are stable: `D9a`-`D9d` are later insertions that keep the lettered suffix so earlier references to `D9` and `D10` elsewhere in these artifacts stay valid.

| # | Decision | Alternatives rejected |
|---|----------|-----------------------|
| **D1** | The `Tag` type is added to the existing external module `external/vsphere-policy/api/v1alpha1`, group `vsphere.policy.vmware.com`, version `v1alpha1`. Its controller goes under `controllers/vspherepolicy/tag/` and its webhook under `webhooks/vspherepolicy/tag/`. | Adding `Tag` to `api/v1alpha6` under `vmoperator.vmware.com`: rejected because the source spec's data model is explicit about the group/version, the type is conceptually part of the vSphere policy surface that already owns `TagPolicy`, and placing it there keeps it out of the DevOps-facing API group (spec G9). Cost accepted: the type lives in a module whose upstream owner is outside this repository, so future upstream changes must be reconciled. |
| **D2** | The VM reconcile path owns `Tag` resources and vCenter tag application; the `Tag` controller fans out. Concretely: a new `vmconfig` reconciler creates/patches the `Tag`, maintains its **own** VM's owner reference, and emits the `TagSpec` add/remove diff; the `Tag` controller owns `Tag` status, its finalizer, its delete-at-zero-owners decision, and enqueuing every affected VM in the namespace. | Making the `Tag` controller derive ownership for *all* VMs itself (list VMs, compute the full owner set, write it): rejected because it makes the `Tag` controller a second writer of state each VM's own reconcile already computes, duplicating the affinity-term walk and creating a two-writer race on `ownerReferences` with no single authority per entry. The chosen split gives exactly one writer per owner-reference entry. |
| **D3** | A new feature flag `Features.TaggingAPI` in `pkg/config/config.go`, defaulted to `true` in `pkg/config/default.go` on this development branch, with **no** Supervisor capability mapping yet. | Reusing `Features.VMAffinityDuringExecution`: rejected because it would couple this rollout to an already-shipped capability and make flag-off regression testing (spec SC-006) impossible. Gating on a `supports_*` capability now: deferred deliberately (spec NG6) — capability wiring in `pkg/config/capabilities/capabilities.go` is a follow-up. |
| **D4** | `genConfigSpecTagSpecsFromVMLabels` stays and continues to serve the flag-off path; the `Tag`-driven path takes over when the flag is on. The two are mutually exclusive at runtime, never additive. | Deleting the create-time mechanism in this change set: rejected to keep the change set reversible by flag and to keep the existing unit suites meaningful as the flag-off regression baseline. Removal is a follow-up spec (spec NG4). |
| **D5** | The `Tag` resource name is `"tag-" + pkgutil.SHA1Sum17("<key>:<value>")` — a pure hash, following the `VMIName` precedent. Readability is restored with `additionalPrinterColumns` for the label key and value. | Sanitized `<key>-<value>` (with or without a hash suffix): rejected because label keys may be prefixed (`example.com/tier`) and values may be long or empty, so sanitizing needs a collision rule (`a/b=c` vs `a-b=c`) and truncation rules; the hash has neither problem and the printer columns recover the readability that motivated the alternative. |
| **D6** | The vCenter tag name stays `"<key>:<value>"` in category `<namespace>`, byte-for-byte what `affinity.go` emits today. | A new name format: rejected — it would fork the tag namespace from the Compute Policy side (`buildTagIDsFromTopology`), require a coordinated change there, and orphan tags already applied to running VMs. |
| **D7** | `spec.affinity` becomes mutable **only** when `Features.TaggingAPI` is enabled; `validateImmutableVMAffinity` is gated on the flag. | Dropping the immutability check unconditionally: rejected because it changes behavior on flag-off Supervisors and breaks spec G11/SC-006. Leaving it immutable and deferring US3: rejected — US3 is in the epic's scope and the user confirmed the relaxation belongs here. |
| **D8** | `Tag` gets a Go validating webhook at `webhooks/vspherepolicy/tag/validation/`: `spec.label`/`spec.value` required and immutable after create, and create/update/delete restricted to privileged accounts. | CEL-only validation: rejected — CEL can express the immutability transition rule but not "privileged accounts only," which is the rule that actually keeps the API invisible to DevOps users (spec G9). RBAC only: rejected — namespace-scoped default DevOps RBAC in a Supervisor namespace is broad, and admission is the enforcement point the rest of this repository uses for equivalent rules. |
| **D9** | No owner-count column on `kubectl get tags`; printer columns are the label key, the label value, and age. | A `status.ownerCount` field feeding a `VMS` column, or a column rendering `ownerReferences[*].name`: both rejected on the user's call — a name list can be arbitrarily long and wrecks the list output, and a count field is API surface added purely for display. Owner detail remains available via `kubectl get tag -o yaml` (spec US4 scenario 2). |
| **D9a** | Ownership and tag carriage are two **different** predicates: a VM owns a `Tag` iff it carries the label **and** references it from `spec.affinity`; a VM carries the vCenter tag iff it carries the label **and** a `Tag` for the pair exists. Tag carriage never consults the VM's own affinity. | The source spec was internally contradictory here: US2.1 tagged a VM carrying the label with no affinity, while US3.2 untagged a VM carrying the label whose affinity had changed away from it — the same live state (carries the label, is not an owner, another VM owns the `Tag`) with opposite outcomes, distinguishable only by history a level-triggered reconciler does not keep. Rejected alternative 1, "tagged unless the VM declares affinity that excludes the label": would have satisfied both scenarios as written and is history-free, but it silently drops a VM out of a relationship it still carries the label for, defeating the other VMs' stated affinity toward it. Rejected alternative 2, "owners only": would have deleted US2 and with it the feature's main new capability (spec G2). Resolution: label match alone wins, and US3.2 / US3.4 were rewritten in `spec.md` — a VM that stops referencing a label it still carries becomes an ordinary label-only participant. |
| **D9b** | A `Tag` is created only for a pair a VM **both** carries and references. A VM referencing a pair it does not carry creates nothing. | Creating a `Tag` for every referenced pair regardless of carriage: it would tag the affinity *target* set (any VM carrying `app=redis` would be tagged for a VM whose affinity points at it without carrying it), making one-way affinity enforceable. Rejected on the user's call: it widens `Tag` creation to labels no VM in the namespace is available for, and the target set gets tagged anyway as soon as some VM both carries and references the pair. Consequence accepted: affinity toward a pair that no VM both carries and references stays unenforced. |
| **D9c** | `Tag` lookups are served by three mechanisms: a `Get` on the derived name for exact-pair existence (the hot path), field indexes on `spec.label` and `spec.labelKeyValue` for key-wide and name-unknown lookups, and the `metadata.labels` mirror for `kubectl` only. | Relying on the `metadata.labels` mirror with `client.MatchingLabels` for the control plane's own queries: rejected because it is an in-memory filter over the whole namespace, not an index (see above) — it would make the most frequent query in the feature O(all `Tag`s in the namespace) while looking like an indexed lookup in the code. Dropping the mirror entirely: rejected because it is the only way to select `Tag`s by label from `kubectl` and the only thing that makes a hash-named resource legible at a glance. |
| **D9d** | The `Tag` controller's fan-out query — "give me the VMs carrying this `Tag`'s label" — is served by a multi-valued field index `metadata.labels.keyValue` on `vmopv1.VirtualMachine` (one `"<key>:<value>"` entry per non-VM-Operator label), so it returns matches only. The fan-out then enqueues **every** matching VM, accepting that most resulting reconciles are no-ops. | Filtering in memory with `client.MatchingLabels`: rejected — it returns every VM in the namespace from the cache and filters afterward, so a query that runs on every `Tag` create/ownership-change/delete would scale with namespace size instead of relationship size. Having the `Tag` controller enqueue only the VMs whose tag set actually changed: rejected because it would have to reproduce each VM's tag diff against a stale view, duplicating the VM path's logic and re-introducing the two-writer problem D2 exists to avoid; an indiscriminate fan-out plus an idempotent no-op reconcile is both simpler and safer. Workqueue de-duplication and the skip-if-unchanged guard keep the no-op path read-only, so it cannot loop. |
| **D10** | The `Tag` controller carries the `vmoperator.vmware.com/tag` finalizer and, before removing it, enqueues every VM in the namespace matching the `Tag`'s key/value. | Letting the `Tag` disappear immediately on delete: rejected — label-only VMs are not owners, so nothing else would ever tell them to drop the vCenter tag, breaking spec G5 and US2 scenario 2. |

---

## Open follow-ups (not part of this feature)

- Wire `TaggingAPI` to a Supervisor capability key in `pkg/config/capabilities/capabilities.go` and flip the development-branch default back to `false` (spec NG6, D3).
- Remove `genConfigSpecTagSpecsFromVMLabels` and its call sites once the `Tag`-driven path is GA (spec NG4, D4).
- Populate the `Tag`'s reserved vCenter Tag UUID field once something needs the resolved UUID (spec NG1); today's tag attachment is name+category-based and does not.
- Remove the `VirtualMachineGroup` dependency from affinity end to end (spec NG3, SC-004).
