# E2E Test Plan: Tag CRD + Tag Controller for Affinity

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Tasks**: [`tasks.md`](./tasks.md) (T031, T032, T033)

This document specifies the end-to-end suite for this feature: the scenarios the implementation must satisfy against a real Supervisor, organized by the user story in `spec.md` each one validates.

## Suite

- `VMAffinityTagSpec`, in `test/e2e/vmservice/vmservice/virtualmachine/vm_affinity_tag.go`, registered from `test/e2e/vmservice/vmservice_test.go` under a `Context("VM-AFFINITY-TAG", ...)` block. `TEST_FOCUS="VM-AFFINITY-TAG"` selects it.
- Every scenario carries `"core-functional"`, except those that need direct `govmomi` access to read a VM's attached vCenter tags, which carry `"extended-functional"` and skip when that access is unavailable.
- Every scenario also carries `"experimental"` per `e2e-testing.md`, dropped once the suite has been validated on hardware (tasks.md T037).
- New config keys in `test/e2e/vmservice/config/wcp.yaml`: `default/wait-vm-affinity-tag-applied` (5m/10s) and `default/wait-tag-cr-deleted` (3m/10s).
- The suite reads `Tag` resources through the `vsphere.policy.vmware.com/v1alpha1` scheme. No change is needed there: `test/e2e/vmservice/common/scheme.go` calls the module's `AddToScheme`, and `Tag` registers itself via its `init()`.
- VM shapes are built with the existing affinity helpers' pattern from `test/e2e/vmservice/vmservice/virtualmachine/vm_group.go` (`createVMWithAffinityAndAntiAffinityFunc`); label-only VMs use the plain `manifestbuilders` path with labels and no `spec.affinity`.

## Gating

The feature is gated on `Features.TaggingAPI`, which in this change set is a plain feature flag with **no** Supervisor capability backing it (spec NG6), so there is no capability for the suite to query. The `Tag` CRD's presence is used instead. The suite:

- Skips entirely when no `Tag` CRD is registered on the target Supervisor — a `NoKindMatchError` on the first `Tag` list.
- Relies on T002b's `case "Tag":` in `pkgcrd.Install` for that to mean anything: without it the kind falls through to `Install`'s `default:` branch and is installed unconditionally, on every Supervisor, feature or no feature.
- Gets a **one-way** signal even with T002b, and should be read that way. Absence is conclusive — the flag has never been on. Presence is not: CRD deletion on flag-off is additionally gated on `pkgcfg.CRDCleanupEnabled`, which defaults to `false`, so a Supervisor that once had the flag on keeps the CRD after it is turned off. On such a cluster the suite will run and fail rather than skip. That is acceptable for CI, which builds fresh, and is the reason every scenario keeps `"experimental"` until the capability lookup lands.
- Should switch to a capability lookup with a named constant in `test/e2e/vmservice/consts/consts.go` — alongside the existing `VMAffinityDuringExecutionCapabilityName` — once the flag is wired to one (`research.md` "Open follow-ups"). That closes the one-way gap; T037 should not drop `"experimental"` before it does.

Verifying the vCenter-side tag is what actually proves the behavior; asserting only on `Tag` resources would pass even if no tag ever reached a VM. Scenarios that assert the vCenter side read the VM's attached tags directly via `govmomi` and therefore carry `"extended-functional"`.

## Scenarios

### US1 — label referenced by a new VM's affinity becomes a vCenter tag

| Scenario | Verifies |
|----------|----------|
| creates a VM whose affinity references its own label and observes the Tag CR and the vCenter tag | US1.1 — `Tag` exists in the namespace with the derived name, `spec.label`/`spec.value`, the label mirror, and one owner reference to the VM; vCenter tag `<key>:<value>` in category `<namespace>` is attached |
| adds a second VM with the same label and affinity and observes two owners on one Tag CR | US1.2 — exactly one `Tag` resource, two owner references, both VMs tagged in vCenter |
| creates a VM with two labels but affinity referencing only one | US1.3 — a `Tag` exists for the referenced label only; the unreferenced label produces no `Tag` and no vCenter tag (spec G3) |
| deletes one of two owning VMs and observes the Tag CR survives | US1.4 — owner count drops to one, `Tag` is not deleted, the surviving VM keeps its vCenter tag |
| deletes the only owning VM and observes the Tag CR is removed | US1.5 — `Tag` disappears from the namespace within `wait-tag-cr-deleted` |

### US2 — pre-existing labeled VM is tagged when a later VM references the label

| Scenario | Verifies |
|----------|----------|
| tags a pre-existing label-only VM once a later VM references the label | US2.1 — the label-only VM is untagged before the referencing VM exists, and is tagged after; the `Tag` records **only** the referencing VM as an owner |
| untags the label-only VM when the referencing VM is deleted | US2.2 — the vCenter tag is removed from the label-only VM and the `Tag` is deleted, driven by the VM controller's `Tag` watch when the `Tag` starts terminating |
| does not tag a same-labeled VM in a different namespace | US2.3 — namespace isolation: a `Tag` appears only in the referencing VM's namespace, and the other namespace's VM stays untagged (spec G6) |
| tags a label-only VM created after the Tag CR already exists | US2.4 — a newly-created label-only VM picks up the vCenter tag on its first reconcile and is not added as an owner |

### US3 — affinity spec is changed and tagging converges

| Scenario | Verifies |
|----------|----------|
| changes affinity away from a label on the sole owner and observes full cleanup | US3.1 — the VM's vCenter tag is removed, the owner reference is removed, and the `Tag` is deleted even though the VM still exists (the case ordinary GC does not cover) |
| changes affinity on one of two owners and observes the changed VM keeps its tag | US3.2 — the changed VM's owner reference is removed and one owner remains, but the changed VM **keeps** vCenter tag `app:nginx` because it still carries the label. Assert with `Consistently` that the tag is not removed, so a regression that untags it fails rather than racing to a pass |
| removes the last affinity reference and observes both the owner and the label-only VM untagged | US3.3 — the `Tag` is deleted and both VMs lose the vCenter tag |
| changes affinity from one label to another on the same VM | US3.4 — the new label's `Tag` is created and its vCenter tag applied within a single VM update; the old label's `Tag` is deleted and its vCenter tag removed **because** the VM was its only owner |
| changes affinity from one label to another while a second VM owns the old label | US3.4's qualified branch — the VM gains the new tag and **keeps** the old one, since the old label's `Tag` survives and the VM still carries that label |
| drops both the label and its affinity reference | US3.5 — a VM that removes the label itself ends up carrying only the new tag, with no label-only participation in the old one |
| patches affinity on a running VM and observes it is admitted | SC-005, US3 — admission accepts the `spec.affinity` change with the feature enabled, and convergence needs no power cycle or re-create |

### US4 — CSP admin diagnosis and DevOps-user invisibility

| Scenario | Verifies |
|----------|----------|
| lists Tag resources and reads the label key and value from the default output | US4.1 — the `Label` and `Value` printer columns are populated (spec G10) |
| rejects a Tag create by a DevOps user | US4.3 — admission denies a non-privileged account; assert on the rejection message, not just the error type |
| rejects a change to a Tag's label or value by a privileged account | US4.4 — immutability rules V3/V4 |

### Edge cases from `spec.md`

| Scenario | Edge case covered |
|----------|--------------------|
| re-creates the Tag CR when a VM needs it while the previous one is still terminating | Tag deletion racing a re-create (spec G12) — delete the sole owner and immediately create a new VM with the same label and affinity; the `Tag` must end up present with the new VM as owner, and the new VM must end up tagged |
| keeps a same-key different-value label untagged | A `Tag` represents key **and** value; a VM carrying the key with another value is unrelated |
| applies the tag independently of VirtualMachineGroup membership | SC-004 — two VMs in **different** `VirtualMachineGroup`s sharing the same affinity label both carry the same vCenter tag |
| leaves tags in place when the VM is re-created | A delete/re-create cycle of a participating VM converges back to the same `Tag` and the same vCenter tag |

## Scope boundaries (not E2E-tested by design)

- Flag-off behavior (spec SC-006) is unit-tested only. Toggling `Features.TaggingAPI` requires restarting the controller manager on the Supervisor, which is outside what this suite does; the flag-off path's equivalence to today's behavior is asserted in `pkg/providers/vsphere/virtualmachine/` unit tests (tasks.md T010).
- The concurrent-owner-reference race (two VMs patching the same `Tag` simultaneously) is covered by the envtest integration test (tasks.md T027), not E2E — reliably interleaving two reconciles against a real Supervisor is not achievable, while envtest can drive both writes deterministically.
- `status.id` remaining empty (spec NG1) needs no scenario: it is an absence, already asserted at the unit level, with no cluster-observable consequence.
- Admission rule V5 (resource name must equal the derived name) is unit-tested only; it can only be triggered by hand-authoring a `Tag`, which no supported workflow does.
- Unsupported label-selector operators being ignored rather than fatal is unit-tested only — it has no cluster-observable effect beyond the absence of a tag.
- The two accepted limitations of diffing against the ExtraConfig record rather than the live attached-tag list (spec "Edge cases": a tag detached out of band is not re-applied, and a remove may be emitted for a tag already gone) are not E2E-tested. The first would require detaching a tag directly in vCenter to assert that VM Operator does *not* react, which is a test that passes for the wrong reasons; the second has no observable effect. Both are covered at the unit level (tasks.md T014a).
