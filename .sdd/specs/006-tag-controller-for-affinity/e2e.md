# E2E Test Plan: Tag CRD + Tag Controller for Affinity

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Tasks**: [`tasks.md`](./tasks.md) (T031, T032, T033)

This document specifies the end-to-end suite for this feature: the scenarios the implementation must satisfy against a real Supervisor, organized by the user story in `spec.md` each one validates.

## Suite

- `VMAffinityTagSpec`, in `test/e2e/vmservice/vmservice/virtualmachine/vm_affinity.go`, registered from `test/e2e/vmservice/vmservice_test.go` under a `Context("VM-AFFINITY-TAG", ...)` block. `TEST_FOCUS="VM-AFFINITY-TAG"` selects it.
- Every scenario carries `"core-functional"`, except those that need direct `govmomi` access to read a VM's attached vCenter tags, which carry `"extended-functional"` and skip when that access is unavailable.
- Every scenario also carries `"experimental"` per `e2e-testing.md`, dropped once the suite has been validated on hardware (tasks.md T037).
- New config keys in `test/e2e/vmservice/config/wcp.yaml`: `default/wait-vm-affinity-tag-applied` (60s/10s) and `default/wait-tag-cr-deleted` (3m/10s).
- The suite reads `Tag` resources through the `vsphere.policy.vmware.com/v1alpha1` scheme. No change is needed there: `test/e2e/vmservice/common/scheme.go` calls the module's `AddToScheme`, and `Tag` registers itself via its `init()`.
- VM shapes are built with the existing affinity helpers' pattern from `test/e2e/vmservice/vmservice/virtualmachine/vm_group.go` (`createVMWithAffinityAndAntiAffinityFunc`); label-only VMs use the plain `manifestbuilders` path with labels and no `spec.affinity`.

## Gating

The feature is gated on `Features.TaggingAPI`, which defaults to false and is set from `CapabilityKeyVMHardAffinityDuringExecution` (`supports_vm_service_vm_hard_affinity_during_execution`) in `pkg/config/capabilities/capabilities.go` (spec G13).

- Both Tag suites use `SkipUnlessTaggingAPIEnabled` to query the backing Supervisor capability through the established capability helper, supporting the legacy ConfigMap and async-upgrade Capabilities resource. No TaggingAPI deployment environment variable is required.
- The affinity-tag suite also requires VM Groups and affinity during execution for its VM manifests.
- The affinity-tag suite skips when the first `Tag` list returns a `NoKindMatchError`; capability activation does not prove the operator has installed the CRD yet. CRD presence alone is insufficient because the CRD may remain after the feature is disabled when `CRDCleanupEnabled` is false.
- Keep `"experimental"` until the scenarios are validated on a real WCP cluster.
- US1.4 is active and experimental, asserting target-tag provisioning from its create-time `VmToVmGroupsAntiAffinity` policy and attachment to the subsequent carrier. It additionally requires `VMPlacementPolicies` and a topology supporting zone anti-affinity. Local tests assert policy emission with TaggingAPI enabled and policy omission when `VMPlacementPolicies` is disabled.

Verifying the vCenter-side tag is what actually proves the behavior; asserting only on `Tag` resources would pass even if no tag ever reached a VM. Scenarios that assert the vCenter side read the VM's attached tags directly via `govmomi` and therefore carry `"extended-functional"`.

## Scenarios

### US1 — label referenced by a new VM's affinity becomes a vCenter tag

| Scenario | Verifies |
|----------|----------|
| creates a VM whose affinity references its own label and observes the Tag CR and the vCenter tag | US1.1 — `Tag` exists in the namespace with the derived name, `spec.key`/`spec.value`, the label mirror, and one owner reference to the VM; vCenter tag `<key>:<value>` in category `<namespace>` is attached |
| adds a second VM with the same label and affinity and observes two owners on one Tag CR | US1.2 — exactly one `Tag` resource, two owner references, both VMs tagged in vCenter |
| creates a VM with two labels but affinity referencing only one | US1.3 — a `Tag` exists for the referenced label only; the unreferenced label produces no `Tag` and no vCenter tag (spec G3) |
| creates a VM whose affinity (a `VmToVmGroupsAntiAffinity`-style term) references a label the VM does not itself carry | US1.4 — a `Tag` is created with the VM as sole owner even though it does not carry the pair; the VM itself is **not** given the vCenter tag; a second VM created afterward carrying that label **is** tagged once the `Tag` exists (US2 mechanics) |
| deletes one of two owning VMs and observes the Tag CR survives | US1.5 — owner count drops to one, `Tag` is not deleted, the surviving VM keeps its vCenter tag |
| deletes the only owning VM and observes the Tag CR is removed | US1.6 — `Tag` disappears from the namespace within `wait-tag-cr-deleted` |

### US2 — pre-existing labeled VM is tagged when a later VM references the label

| Scenario | Verifies |
|----------|----------|
| tags a pre-existing label-only VM once a later VM references the label | US2.1 — the label-only VM is untagged before the referencing VM exists, and is tagged after; the `Tag` records **only** the referencing VM as an owner |
| untags the label-only VM when the referencing VM is deleted | US2.2 — the vCenter tag is removed from the label-only VM and the `Tag` is deleted, and the untagging is driven by the VM controller's `Tag` watch reacting to that delete event |
| does not tag a same-labeled VM in a different namespace | US2.3 — namespace isolation: a `Tag` appears only in the referencing VM's namespace, and the other namespace's VM stays untagged (spec G6) |
| tags a label-only VM created after the Tag CR already exists | US2.4 — a newly-created label-only VM picks up the vCenter tag on its first reconcile and is not added as an owner |

### US3 — label participation changes and tag carriage converges

`spec.affinity` is immutable (spec NG8), so these scenarios change a running VM's **labels**. Ownership follows the affinity reference alone, so dropping a label never drops ownership — the VM's `spec.affinity` still references the pair and cannot change — only that VM's own vCenter tag carriage reacts.

| Scenario | Verifies |
|----------|----------|
| drops the label on the sole owner and observes the Tag CR survive while only the vCenter tag is removed | US3.2, SC-005 — the VM's vCenter tag is removed, but its owner reference is **unaffected** and the `Tag` is **not** deleted (its `spec.affinity` still references the pair), with no power cycle or re-create |
| gains a second referenced label while retaining the first tag | US3.4 — create with both labels and immutable affinity references to establish both vCenter tags through supported policy workflows; remove the second label and wait for detachment, then gain it again. Both tags attach, both Tag CR identities and ownership remain unchanged, and a peer retains the first tag |
| swaps referenced labels while a peer retains the dropped tag | US3.5 — establish both tags at creation, remove the second label, then drop the first and gain the second in one update. Only the second tag remains on the changed VM; both Tag CR identities and ownership remain unchanged, and a peer retains the first tag |
| drops the label on one of two owners and observes both owners and the label-only VM keep their Tag-CR standing | US3.1 and US3.3 — the relabeled VM's owner reference is **unaffected** and both owners remain; the relabeled VM loses its vCenter tag because it no longer carries the label, the surviving owner keeps its own vCenter tag, and a third label-only VM **keeps** its tag. Assert the surviving owner and the label-only VM with `Consistently`, so a regression that untags either fails rather than racing to a pass |

The label-gain and label-swap scenarios also verify unchanged Kubernetes UID, vCenter MoID, immutable affinity, and powered-on state.

### Edge cases from `spec.md`

| Scenario | Edge case covered |
|----------|--------------------|
| re-creates a deleted Tag CR with the same name and a new UID for a replacement owner | Recovery after Tag disappearance (spec G12) — delete the sole owner, confirm the Tag GET returns NotFound, then immediately re-create the VM with the same label and affinity in its dedicated group. Assert a new VM UID, the same derived Tag name with a different Tag UID, only the replacement owner UID, and vCenter tag attachment. This deterministic sequence proves actual recreation. The cached-read/deletion/ownership-write interleaving is covered deterministically in `pkg/vmconfig/vmtags/vmtags_reconciler_test.go` |
| converges two VMs created at once on a single Tag CR | Tag create racing a create (spec G12) — create two VMs carrying and referencing the same label pair in one go; exactly one `Tag` must exist for the pair, with both VMs as owners and both tagged in vCenter |
| keeps a same-key different-value label untagged | A `Tag` represents key **and** value; a VM carrying the key with another value is unrelated |
| applies the tag independently of VirtualMachineGroup membership | SC-004 — two VMs in **different** `VirtualMachineGroup`s sharing the same affinity label both carry the same vCenter tag |
| leaves tags in place when the VM is re-created | A delete/re-create cycle of a participating VM converges back to the same `Tag` and the same vCenter tag |

### Skip-delete platform cleanup

| Scenario | Verifies |
|----------|----------|

## Scope boundaries (not E2E-tested by design)

- Flag-off behavior (spec SC-006) is unit-tested only. Toggling `Features.TaggingAPI` requires restarting the controller manager on the Supervisor, which is outside what this suite does; the flag-off path's equivalence to today's behavior is asserted in `pkg/providers/vsphere/virtualmachine/` unit tests (tasks.md T010).
- G12 uses deterministic coverage at each layer: the deletion/recreation E2E proves recovery after confirmed disappearance; the concurrent-create E2E checks cluster convergence; reconciler tests force `AlreadyExists` adoption and deletion between a cached Get and its ownership patch, asserting that the failed write is returned for retry and the next reconcile creates the replacement. No additional timing-dependent deletion/replacement E2E is required.
- The concurrent-owner-reference race (two VMs patching the same `Tag` simultaneously) is covered by the envtest integration test (tasks.md T027), not E2E — reliably interleaving two reconciles against a real Supervisor is not achievable, while envtest can drive both writes deterministically.
- `status.id` remaining empty (spec NG1) needs no scenario: it is an absence, already asserted at the unit level, with no cluster-observable consequence.
- Admission rule V5 (resource name must equal the derived name) is unit-tested only; it can only be triggered by hand-authoring a `Tag`, which no supported workflow does.
- Unsupported label-selector operators being ignored rather than fatal is unit-tested only — it has no cluster-observable effect beyond the absence of a tag.
- The two accepted limitations of diffing against the ExtraConfig record rather than the live attached-tag list (spec "Edge cases": a tag detached out of band is not re-applied, and a remove may be emitted for a tag already gone) are not E2E-tested. The first would require detaching a tag directly in vCenter to assert that VM Operator does *not* react, which is a test that passes for the wrong reasons; the second has no observable effect. Both are covered at the unit level (tasks.md T014a).

## Initial placement regression (vmop-3882)

The existing `vm_group.go` scenarios “Should create 3 VMs with required host AAF with each other” and “Should place VMs without a group on different hosts using host anti-affinity” guard initial placement. Run them with TaggingAPI enabled in a fresh namespace and confirm distinct host placements without pre-creating Tag resources. Repeat with TaggingAPI disabled as the compatibility baseline. Unit coverage additionally checks fresh host/zone affinity and anti-affinity, union with existing-resource tags, and deduplication of inherited create additions. Real-cluster validation of this fix remains pending.
