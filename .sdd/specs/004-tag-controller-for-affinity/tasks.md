# Tasks: Tag CRD + Tag Controller for Affinity

- **Spec**: [`spec.md`](./spec.md)
- **Plan**: [`plan.md`](./plan.md)
- **Model**: [`model.md`](./model.md)
- **E2E plan**: [`e2e.md`](./e2e.md)
- **Epic**: vmop-3882

<!--
The [vmop-NNN] tags below are allocated from vmop-11000 upward, in task order. Each one must exist as a story or sub-task and be linked to epic vmop-3882 via customfield_10830 (Epic Link), set post-create via PUT — the JIRA create screen does not expose it. Check off each task as it lands, and add any task discovered mid-implementation.
-->

## Phase 1 — Setup

- [ ] T001 [vmop-11000] Add `Tag`, `TagList`, `TagSpec`, `TagStatus`, the `Ready` reason constants, and the printer-column / root / storageversion / status-subresource markers in `external/vsphere-policy/api/v1alpha1/tag_types.go`, appending both types to `objectTypes` from `init()` — schema and markers per [`model.md`](./model.md) "Schema"
- [ ] T002 [vmop-11001] Regenerate deepcopy and the external CRD manifest (`make generate-go generate-external-manifests`) — `external/vsphere-policy/api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/external-crds/vsphere.policy.vmware.com_tags.yaml`; note the new CRD in `config/crd/external-crds/README.md`. No Makefile change is needed (the target already globs the module), and no envtest change is needed (`test/builder/test_suite.go` loads the whole `config/crd/external-crds` directory)
- [ ] T003 [P] [vmop-11002] Add `Features.TaggingAPI` to `pkg/config/config.go` and seed it `true` in `pkg/config/default.go` for this development branch (no capability mapping — spec NG6)
- [ ] T004 [P] Register `&vspherepolv1.Tag{}` in `KnownObjectTypes` in `test/builder/fake.go` so the fake client enforces the status-subresource split

## Phase 2 — Foundational

- [ ] T005 [vmop-11003] Declare the four field-index keys and their extractor functions in `pkg/util/kube/affinitytag.go`, following the `VMSpecVolumesPVCsIndexKey`/`VMSpecVolumesPVCsIndexerFunc` convention in `pkg/util/kube/pvc.go`: `spec.label`, `spec.labelKeyValue`, and `metadata.ownerReferences.uid` on `vspherepolv1.Tag`, plus the multi-valued `metadata.labels.keyValue` on `vmopv1.VirtualMachine` (one `"<key>:<value>"` per label after `kubeutil.RemoveVMOperatorLabels`; the extractor MUST NOT mutate the cached object)
- [ ] T006 [P] [vmop-11004] Unit tests for the extractors in `pkg/util/kube/affinitytag_test.go`: multi-label VMs, reserved-label filtering, empty values, prefixed keys, a VM with no labels, and a `Tag` with several owner references
- [ ] T007 [vmop-11005] Implement the shared helpers `AffinityLabelPairs`, `TagResourceName`, and `VCenterTagName` in `pkg/util/vmopv1/affinitytag.go`; refactor `extractAffinityLabelsFromVM` in `pkg/providers/vsphere/virtualmachine/affinity.go` to call `AffinityLabelPairs` so the flag-on and flag-off paths cannot diverge on what "referenced by affinity" means
- [ ] T008 [P] [vmop-11006] Unit tests for the shared helpers (selector operators, topology-key eligibility per `AffinityRuleConstraints`, prefixed keys, empty values, name determinism) in `pkg/util/vmopv1/affinitytag_test.go`
- [ ] T009 [vmop-11007] Gate the create-time mechanism off when the flag is on, at both `genConfigSpecTagSpecsFromVMLabels` call sites in `pkg/providers/vsphere/virtualmachine/configspec.go` (`CreateConfigSpec`, `CreateConfigSpecForPlacement`); leave `genConfigSpecAffinityPolicies` untouched — see [`plan.md`](./plan.md) "Flag-off path preserved"
- [ ] T010 [P] [vmop-11008] Extend the existing affinity/configspec unit tests so the flag-off path is asserted byte-for-byte unchanged and the flag-on path emits no create-time tag specs (spec SC-006) — `pkg/providers/vsphere/virtualmachine/affinity_test.go`, `pkg/providers/vsphere/virtualmachine/configspec_test.go`

## Phase 3 — User Story 1 (DevOps user: affinity-referenced label becomes a vCenter tag)

- [ ] T011 [US1] [vmop-11009] Implement the `vmconfig.Reconciler` in `pkg/vmconfig/affinitytag/affinitytag_reconciler.go`: ensure-`Tag`-exists, own-ownerReference add/remove under `MergeFromWithOptimisticLock` with a skip-if-unchanged guard, desired-tag-set computation, and the `TagSpec` add/remove diff against `moVM` — steps 1-5 of [`plan.md`](./plan.md) "VM path". The owned-pair set (labels ∩ affinity) drives ownership only; the tag diff is driven by labels ∩ existing `Tag`s and MUST NOT consult the VM's own `spec.affinity`
- [ ] T012 [US1] [vmop-11010] Register the reconciler behind `Features.TaggingAPI` in `controllers/virtualmachine/virtualmachine/virtualmachine_controller.go`, and add the `vsphere.policy.vmware.com/tags` RBAC marker there
- [ ] T013 [US1] [vmop-11011] Release ownership on the VM delete path (before the VM's finalizer is removed) in `pkg/vmconfig/affinitytag/affinitytag_reconciler.go`
- [ ] T014 [P] [US1] [vmop-11012] Unit tests for the reconciler in `pkg/vmconfig/affinitytag/affinitytag_reconciler_test.go` + suite bootstrap `affinitytag_reconciler_suite_test.go`: `Tag` create shape, second owner appended not replaced, owner removed on affinity change and on VM delete, mismatched-spec collision error, no-op reconcile writes nothing, step 4 issuing one indexed `List` per distinct label key rather than an unfiltered namespace list, and — per spec "Ownership vs. tag carriage" — a pair referenced but not carried creates no `Tag`, while a VM that stops referencing a label it still carries keeps the tag as long as another owner remains
- [ ] T015 [US1] [vmop-11013] Integration test (vcsim) that the emitted `TagSpec`s reach the reconfigure call and an attached-tag diff round-trips, in `pkg/providers/vsphere/session/session_vm_update_test.go`

## Phase 4 — User Story 2 (label-only VM is tagged when a later VM references the label)

- [ ] T016 [US2] [vmop-11014] Implement the `Tag` controller in `controllers/vspherepolicy/tag/tag_controller.go`: canonical reconcile loop, `vmoperator.vmware.com/tag` finalizer, label-mirror correction, `status.observedGeneration` + `Ready`, and `cource` fan-out via an **indexed** `List` (`client.InNamespace` + `client.MatchingFields{metadata.labels.keyValue}`) — not `client.MatchingLabels`, which filters in memory after listing the namespace; register all four field indexes from T005 in `AddToManager`; add the `tags`, `tags/status`, **and** `virtualmachines` (`get;list;watch`, needed for the fan-out `List`) RBAC markers
- [ ] T017 [US2] [vmop-11015] Register the `Tag` controller behind the flag in `controllers/vspherepolicy/controllers.go`
- [ ] T018 [US2] [vmop-11016] Implement `ReconcileDelete`: fan out **before** releasing the finalizer, so label-only VMs drop the vCenter tag (spec G5)
- [ ] T019 [P] [US2] [vmop-11017] Unit tests for the controller in `controllers/vspherepolicy/tag/tag_controller_test.go` + suite bootstrap `tag_controller_suite_test.go`: finalizer lifecycle, status, label-mirror correction, fan-out set (matching VMs including label-only, no event for non-matching), delete-path ordering
- [ ] T020 [P] [US2] [vmop-11018] Unit test that a label-only VM receives `TagSpec{add}` and is **not** added as an owner, in `pkg/vmconfig/affinitytag/affinitytag_reconciler_test.go`
- [ ] T021 [P] [US2] [vmop-11019] Integration test (envtest) that each field index resolves as specified, in `controllers/vspherepolicy/tag/tag_controller_test.go`: `spec.label` returns every `Tag` for a key, `spec.labelKeyValue` returns the exact pair, `metadata.ownerReferences.uid` returns a VM's owned `Tag`s, and `metadata.labels.keyValue` returns exactly the label-carrying VMs — not same-key-different-value VMs, and not VMs in other namespaces
- [ ] T022 [P] [US2] [vmop-11020] Test the no-op fan-out property: a VM enqueued by a `Tag` change whose tag set already matches the attached tags emits no `TagSpec`, issues no reconfigure, and patches neither the VM nor any `Tag` — so a fan-out cannot bump `resourceVersion` and re-trigger watchers. Unit level in `pkg/vmconfig/affinitytag/affinitytag_reconciler_test.go`, plus a `Consistently` guard at the envtest level in `controllers/vspherepolicy/tag/tag_controller_test.go`

## Phase 5 — User Story 3 (affinity spec changes and tagging converges)

- [ ] T023 [US3] [vmop-11021] Gate `validateImmutableVMAffinity` on `Features.TaggingAPI` in `webhooks/virtualmachine/validation/virtualmachine_validator.go`, leaving `validateVMAffinity`'s shape checks applying to the new value on update
- [ ] T024 [P] [US3] [vmop-11022] Unit tests for affinity mutability (mutable flag-on, rejected flag-off) in `webhooks/virtualmachine/validation/virtualmachine_validator_test.go`
- [ ] T025 [US3] [vmop-11023] Implement delete-at-zero-owners in the `Tag` controller (`controllers/vspherepolicy/tag/tag_controller.go`) — the case ordinary garbage collection does not cover; see [`plan.md`](./plan.md) "Complexity tracking"
- [ ] T026 [US3] [vmop-11024] Handle the deletion/re-create race in `pkg/vmconfig/affinitytag/affinitytag_reconciler.go`: a `Tag` found with a non-zero `deletionTimestamp` yields `pkgerr.RequeueError` — never adopted, never silently skipped (spec G12)
- [ ] T027 [P] [US3] [vmop-11025] Integration tests (envtest) in `controllers/vspherepolicy/tag/tag_controller_test.go`: real GC deletes the `Tag` when the last owning VM is deleted, does not when one of two owners is deleted, and two concurrent owner-reference writers both survive

## Phase 6 — User Story 4 (CSP admin diagnosis, and the API stays invisible to DevOps users)

- [ ] T028 [US4] [vmop-11026] Implement the validating webhook (rules V1-V6 from [`model.md`](./model.md) "Admission rules") in `webhooks/vspherepolicy/tag/validation/tag_validator.go`, with `webhooks/vspherepolicy/webhooks.go` registering it from `webhooks/webhooks.go` behind the flag
- [ ] T029 [P] [US4] [vmop-11027] Unit tests for every admission rule, positive and negative, including the privileged-vs-DevOps split and the derived-name check, in `webhooks/vspherepolicy/tag/validation/tag_validator_test.go` + suite bootstrap
- [ ] T030 [P] [US4] [vmop-11028] Verify the generated printer columns and status subresource behave as specified, in the envtest portion of `controllers/vspherepolicy/tag/tag_controller_test.go`

## Phase 7 — E2E

- [ ] T031 [US1] [US2] [vmop-11029] Add the E2E suite `VMAffinityTagSpec` in `test/e2e/vmservice/vmservice/virtualmachine/vm_affinity_tag.go`, registered from `test/e2e/vmservice/vmservice_test.go` under `Context("VM-AFFINITY-TAG", ...)`, covering the US1 and US2 scenario tables in [`e2e.md`](./e2e.md)
- [ ] T032 [US3] [US4] [vmop-11030] Extend that suite with the US3 (affinity mutation, `Tag` cleanup, label-only participation after an affinity change) and US4 (admission rejection for a DevOps user) scenarios from [`e2e.md`](./e2e.md)
- [ ] T033 [P] [vmop-11031] Add the wait-interval keys the suite needs to `test/e2e/vmservice/config/wcp.yaml`, any consts additions to `test/e2e/vmservice/consts/consts.go`, and — if the suite introduces a new label or config knob — the corresponding note in `test/e2e/README.md` per `e2e-testing.md`. No scheme change is needed: `test/e2e/vmservice/common/scheme.go` calls the module's `AddToScheme`, which picks `Tag` up via its `init()`

## Phase Final — Polish

- [ ] T034 [vmop-11032] Run `make generate-manifests` and check in the regenerated `config/rbac/role.yaml` and webhook manifests
- [ ] T035 [vmop-11033] Add release notes per `pull-request-standards.md`: namespace-wide affinity tagging independent of `VirtualMachineGroup`, and `spec.affinity` becoming mutable when the feature is enabled
- [ ] T036 Flip `spec.md` status to `Implemented` once every acceptance criterion is covered (the `.sdd/INDEX.md` row for 004 already exists; update its Status column in the same change)
- [ ] T037 Remove `"experimental"` from the E2E labels once the suite has been validated on a real Supervisor (per `e2e-testing.md`)

## Deferred (tracked as follow-ups, not part of this change set)

Recorded in [`research.md`](./research.md) "Open follow-ups" — each needs its own spec or story before it is picked up:

- Wire `TaggingAPI` to a Supervisor capability key and return the default to `false` (spec NG6).
- Remove `genConfigSpecTagSpecsFromVMLabels` and its call sites once the `Tag`-driven path is GA (spec NG4).
- Populate the reserved vCenter Tag UUID field on `Tag` status (spec NG1).
- Remove the `VirtualMachineGroup` dependency from affinity end to end (spec NG3).
