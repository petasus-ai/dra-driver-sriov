# Design: Advertising Physical Functions (PFs) for Whole-NIC Passthrough

**Status:** Proposed
**Date:** 2026-08-19
**Authors:** Jian Li

## Overview

This design extends discovery and the opt-in advertisement model (see
[opt-in-advertisement.md](opt-in-advertisement.md)) so that a
`SriovResourcePolicy` can advertise SR-IOV-capable **Physical Functions
(PFs)** as allocatable devices, in addition to the Virtual Functions (VFs)
the driver advertises today. PF advertisement is strictly opt-in per
`ResourceFilter`; existing policies and the default behavior are unchanged.

## Motivation

### Current Behavior

`DiscoverSriovDevices()` (`pkg/devicestate/discovery.go`) enumerates PFs only
as *parents*:

1. Net-class PCI devices are scanned; VFs are skipped at the top level
   (`IsSriovVF` → continue), leaving PF candidates.
2. A PF candidate is dropped unless it has a netdev name
   (`TryGetPFInterfaceName(...) == ""` → skip).
3. For each surviving PF, `GetVFList(pf)` is enumerated and **only the VFs**
   are added to the allocatable device list.

A PF is therefore never itself allocatable. Worse, a PF bound to `vfio-pci`
has no netdev, so it is dropped at step 2 and its existence is invisible to
the driver entirely.

### The Gap: Whole-PF Passthrough

Passing an entire NIC through to a virtual machine is a long-standing
pattern, and the default one for InfiniBand HCAs: an IB fabric port is
commonly consumed as a whole PF bound to `vfio-pci` (no VFs configured,
`sriov_numvfs=0`), because the workload wants the full-bandwidth,
SM-managed port rather than a slice of it.

`sriov-network-device-plugin` supports this today: its selectors can match a
PF directly (by `pciAddresses`, `pfNames`, `linkTypes`) and advertise it as an
extended resource. Since a stated goal of the opt-in redesign is for this
driver to serve as a **drop-in DRA-based replacement for
sriov-device-plugin**, PF advertisement is required for parity. Without it, a
cluster whose NAD estate includes whole-PF passthrough networks (e.g. IB
HCAs consumed by KubeVirt VMs through Multus + sriov-cni in passthrough
mode) cannot migrate to the DRA path at all: on such nodes the driver
publishes an empty ResourceSlice, and every consumer keyed on the slice —
DeviceClass CEL selectors, schedulers, capacity/inventory APIs — sees zero
devices.

### Proposed Behavior

1. Discovery also records PF candidates as potential devices, including
   PFs without a netdev (e.g. already bound to `vfio-pci`).
2. A PF is **advertised only** when a `ResourceFilter` explicitly opts in
   via a new `deviceType: pf` field. Filters without the field keep their
   current, VF-only meaning — including the "empty filters match all
   devices" rule, which continues to match VFs only.
3. A PF with configured VFs (`sriov_numvfs > 0`) is never advertised as an
   allocatable device, so a PF and its VFs cannot be handed out at the
   same time.
4. Allocation and preparation reuse the existing attribute-driven paths
   unchanged.

## Goals

- Advertise PFs selected by an explicitly opting-in `SriovResourcePolicy`
  filter, with the same attribute set and lifecycle as VFs.
- Discover PFs that have no netdev (vfio-bound), which are invisible today.
- Guarantee mutual exclusion between a PF and its VFs.
- No behavior change for existing policies (VF-only default).

## Non-Goals

- Advertising non-SR-IOV-capable NICs (out of scope; net-class scan
  boundaries are unchanged).
- Creating or destroying VFs on a PF (`sriov_numvfs` management stays with
  the operator / sriov-network-operator).
- Protecting the node's own uplink beyond the opt-in requirement (see
  Security Considerations).
- Changes to STANDALONE-mode CNI invocation semantics.

## Design Details

### 1. API: `deviceType` on `ResourceFilter`

**File:** `pkg/api/sriovdra/v1alpha1/api.go`

```go
// DeviceType selects the function type a ResourceFilter matches.
// +kubebuilder:validation:Enum=vf;pf
type DeviceType string

const (
    DeviceTypeVF DeviceType = "vf"
    DeviceTypePF DeviceType = "pf"
)

type ResourceFilter struct {
    // DeviceType selects whether this filter matches virtual functions
    // ("vf", the default) or physical functions ("pf"). PF advertisement
    // is strictly opt-in: a filter without deviceType never matches a PF.
    DeviceType     DeviceType `json:"deviceType,omitempty"`
    Vendors        []string   `json:"vendors,omitempty"`
    Devices        []string   `json:"devices,omitempty"`
    PciAddresses   []string   `json:"pciAddresses,omitempty"`
    PfNames        []string   `json:"pfNames,omitempty"`
    PfPciAddresses []string   `json:"pfPciAddresses,omitempty"`
    Drivers        []string   `json:"drivers,omitempty"`
    LinkType       string     `json:"linkType,omitempty"`
}
```

Defaulting to `vf` (empty value) preserves every existing policy verbatim.
Putting the field on `ResourceFilter` rather than `Config` lets one policy
config advertise a mixed pool (e.g. IB PFs plus Ethernet VFs under one
`DeviceAttributes` selection) while keeping each filter unambiguous.

Existing filter keys read naturally against a PF: `vendors`/`devices` match
the PF's own IDs, `pciAddresses` matches the PF address, and
`pfNames`/`pfPciAddresses` match the PF itself (a PF is its own physical
function — see attribute mapping below). `linkType: infiniband` selects IB
ports. Overloading `pfNames` alone ("this PF's VFs" vs. "this PF itself")
was rejected as ambiguous; `deviceType` makes the intent explicit.

Example — advertise two IB HCA PFs for whole-NIC passthrough:

```yaml
apiVersion: sriovnetwork.k8snetworkplumbingwg.io/v1alpha1
kind: SriovResourcePolicy
metadata:
  name: ib-pf-passthrough
  namespace: dra-driver-sriov
spec:
  configs:
  - deviceAttributesSelector:
      matchLabels:
        pool: ib-pf
    resourceFilters:
    - deviceType: pf
      linkType: infiniband
      pciAddresses: ["0000:19:00.0", "0000:19:00.1"]
```

### 2. Discovery Changes

**File:** `pkg/devicestate/discovery.go`

The PF candidate loop changes in two ways:

1. **Netdev-less PFs are kept.** The hard skip on
   `TryGetPFInterfaceName(...) == ""` becomes a soft condition: the PF is
   retained with an empty `NetName`. A vfio-bound PF has no netdev by
   construction, and it is precisely the device this design targets.
   `GetNicSriovMode` (devlink by PCI address) and the sysfs-based helpers
   (`GetNumaNode`, `GetPCIeRoot`, `GetLinkType`) do not depend on a netdev.
   Note `GetLinkType` must fall back to a sysfs read for netdev-less
   devices if its current implementation resolves the type through the
   netdev; for an IB HCA the class/driver information in sysfs is
   sufficient.
2. **Each PF candidate is also emitted as a device entry**, alongside (not
   instead of) its VF enumeration, tagged with its function type. The
   device name follows the existing scheme (`PCI address with ':'/'.'
   mapped to '-'`), which is unique per node by construction.

New discovery attribute:

```go
// consts.go
AttributeDeviceType = DriverName + "/deviceType"   // "pf" | "vf"
```

`AttributeDeviceType` is added to `ReservedAttributes` and stamped on **all**
devices (VFs get `"vf"`), so filters and CEL authors can rely on its
presence. Attribute mapping for a PF entry:

| Attribute | PF value |
|---|---|
| `vendor`, `deviceID` | the PF's own IDs |
| `pfDeviceID` | same as `deviceID` (self) |
| `pciAddress`, `resource.kubernetes.io/pciBusID` | PF address |
| `k8s.cni.cncf.io/deviceID` | PF address (Multus device-info) |
| `pfPciAddress` | PF address (self) |
| `PFName` | netdev name, or omitted when netdev-less |
| `vfID` | omitted (no meaningful value; consumers must guard with `"vfID" in attributes`) |
| `EswitchMode`, `linkType`, `pcieRoot`, `dra.net/numaNode` | as for VFs |
| `rdmaCapable` | per `VerifyRDMACapability(pf)`; `false` for a vfio-bound PF (no kernel RDMA device exists) — documented, not special-cased |
| `deviceType` | `"pf"` |

### 3. Advertisement Gating

**Files:** `pkg/controller/resourcepolicycontroller.go`,
`pkg/devicestate/state.go`

Two rules, both enforced at matching time (`deviceMatchesFilter`) and
re-evaluated on every reconcile:

1. **Explicit opt-in.** `deviceMatchesFilter` first compares the filter's
   `deviceType` (defaulted to `vf`) against the device's
   `AttributeDeviceType`. A mismatch is a non-match. Consequently the
   "empty `resourceFilters` matches all devices" rule keeps matching VFs
   only — an empty filter list has no filter carrying `deviceType: pf`, so
   no PF can be swept in accidentally. This is the safety property that
   keeps host uplinks out of ResourceSlices.
2. **PF/VF mutual exclusion.** A PF with `sriov_numvfs > 0` is excluded
   from advertisement even when a `deviceType: pf` filter matches it
   (skipped with a warning log naming the policy). Otherwise allocating
   the PF while its VFs are allocatable (or vice versa) would hand the
   same silicon to two consumers. VF counts change only through operator
   action, and every such reconfiguration already triggers rediscovery /
   republish, so the exclusion follows the same lifecycle as VF
   inventory changes.

No changes to `UpdatePolicyDevices`, attribute merging, or the
policy/discovery attribute split: a PF is just another allocatable device
once matched.

### 4. Allocation and Preparation

**File:** `pkg/devicestate/state.go` — no structural changes.

`applyConfigOnDevice` is already attribute-driven:

- It reads the target from `AttributePciAddress`; a PF address works
  identically.
- `VfConfig.Driver: vfio-pci` binds the PF (no-op when already bound) and
  injects the `/dev/vfio/*` device nodes — exactly what whole-PF
  passthrough needs. The `VfConfig` name is kept for API compatibility;
  its doc comment gains a note that it applies to any allocated function.
- In MULTUS mode, `extractMultusDeviceInfoAttrs` finds
  `k8s.cni.cncf.io/deviceID` (stamped at discovery) and
  `k8s.cni.cncf.io/resourceName` (stamped via `DeviceAttributes`), so the
  device-info file for Multus is produced unchanged; sriov-cni /
  sriov-vfio-cni treat a PF deviceID as a passthrough device.
- Checkpointing, unprepare, and driver restore all key on PCI address.

One guard is required: any code path reading `AttributeVFID` must tolerate
its absence (PF entries omit it). Today that attribute is informational
only; an audit of readers is part of the implementation.

### 5. What Consumers See

A PF entry differs from a VF entry only in `deviceType`, the absent `vfID`,
and self-referential parent fields. Existing DeviceClass CEL selectors that
match on policy-applied attributes (e.g.
`device.attributes["k8s.cni.cncf.io"].resourceName == "..."`) match PF
entries with no change — which is the point: inventory APIs, schedulers, and
claim templates built against the VF contract work for PFs as-is. Selectors
that must distinguish can test
`device.attributes["sriovnetwork.k8snetworkplumbingwg.io"].deviceType`.

## Edge Cases

- **VFs created on an advertised PF.** Rediscovery sees `sriov_numvfs > 0`,
  rule 3.2 drops the PF from the slice, and its VFs appear (if matched by a
  VF filter). An already-allocated PF claim stays bound — same semantics as
  a policy deletion while allocated (ResourceClaims are immutable once
  allocated); the warning log calls out the conflict.
- **PF carrying node networking.** Nothing prevents an administrator from
  writing `deviceType: pf` with a filter matching the management uplink;
  allocation would then rebind it to vfio and cut the node off. The opt-in
  field is the guard rail; see Security Considerations for the
  recommended narrow-selector practice and a possible follow-up webhook.
- **`drivers` filter.** Driver filtering is not implemented for VFs today
  (`TODO` in `deviceMatchesFilter`); the same limitation applies to PFs
  and is unchanged by this design. When implemented, `drivers:
  ["vfio-pci"]` becomes the natural way to scope PF filters to
  passthrough-prepared devices.

## Testing Strategy

### Unit Tests

1. **Discovery** (`pkg/devicestate/discovery_test.go`, `internal/fakesysfs`)
   - Netdev-less vfio-bound PF is discovered with `deviceType: pf`, empty
     `PFName`, correct self-referential parent attributes.
   - A PF with VFs yields the PF candidate plus its VF entries; VF entries
     carry `deviceType: vf`.
2. **Filter matching** (`pkg/controller/resourcepolicycontroller_test.go`)
   - Default / `vf` filters never match PF entries; `deviceType: pf`
     filters never match VFs.
   - Empty `resourceFilters` matches VFs only.
   - `pciAddresses`, `linkType: infiniband`, `vendors` against PF entries.
3. **Gating** (`pkg/devicestate/state_test.go`)
   - PF with `sriov_numvfs > 0` is not advertised despite a matching
     `deviceType: pf` filter.
4. **Prepare** — PF allocation with `Driver: vfio-pci` produces VFIO device
   nodes and (MULTUS mode) a device-info file keyed on the PF address;
   absence of `vfID` does not fault.

### Integration Tests

1. Node with IB HCAs in PF mode, no policy → empty slice (unchanged).
2. Apply `deviceType: pf` policy + `DeviceAttributes` with
   `k8s.cni.cncf.io/resourceName` → PFs advertised with merged attributes.
3. Allocate a PF claim into a pod/VM → vfio nodes present, device-info
   written, slice shows the device consumed.
4. Set `sriov_numvfs > 0` on an advertised PF → PF leaves the slice, VFs
   enter per VF policy.

## Implementation Plan

1. **API:** add `DeviceType` to `ResourceFilter`; regenerate deepcopy and
   CRD YAMLs; CRD enum validation (`vf|pf`).
   - [x] `DeviceType` type + `NormalizedDeviceType()` on `ResourceFilter`
         (`pkg/api/sriovdra/v1alpha1/api.go`); deepcopy needs no
         regeneration (scalar field, covered by `*out = *in`)
   - [x] CRD schema enum in the Helm template
2. **Discovery:** keep netdev-less PF candidates; emit PF device entries
   with the attribute mapping above; add `AttributeDeviceType` (stamped on
   VFs too) to `consts.go` and `ReservedAttributes`; verify `GetLinkType`
   on netdev-less devices.
   - [x] Netdev-less PFs kept as candidates (empty `NetName`)
   - [x] `buildPFDevice()` emits the PF entry incl. `numVFs`; VF entries
         stamped `deviceType: vf`; `PFName` omitted when netdev-less
   - [x] `GetLinkType` failure falls back to the PCI subclass
         (`linkTypeFromPCISubclass`)
   - [x] `AttributeDeviceType` / `AttributeNumVFs` added to consts and
         `ReservedAttributes`
3. **Matching/gating:** `deviceType` check in `deviceMatchesFilter`;
   `sriov_numvfs` exclusion with warning log.
   - [x] `deviceMatchesFilter` requires filter/device type equality;
         empty `resourceFilters` matches VFs only
   - [x] `getPolicyDeviceMap` refuses a matched PF with `numVFs > 0`
         (warning names the policy). Discovery is startup-only today, so
         VF-count changes take effect on driver restart — same lifecycle
         as VF inventory changes.
4. **Prepare audit:** confirm no reader requires `AttributeVFID`; note on
   `VfConfig` doc comment.
   - [x] Audited: `AttributeVFID` has no readers outside discovery/tests
5. **Tests & docs:** per Testing Strategy; README device-types section;
   cross-reference from opt-in-advertisement.md.
   - [x] Discovery unit tests updated for PF entries + netdev-less /
         subclass-fallback / no-VF cases
   - [x] Controller unit tests for deviceType matching, empty-filter
         VF-only rule, and `numVFs` gating
   - [x] README section + `demo/pf-passthrough/resource-policy-pf.yaml`
   - [ ] Integration tests on a Linux node (unit tests compile via
         `GOOS=linux go vet`; execution requires a Linux environment)

## Security Considerations

PF passthrough hands a whole NIC — potentially the node's own uplink — to a
workload, a strictly larger blast radius than a VF. Mitigations:

1. PF advertisement requires a filter that names `deviceType: pf`
   explicitly; no default, empty, or legacy filter can match a PF.
2. Recommended practice (docs): scope PF filters with `pciAddresses` or
   `pfNames`, not broad vendor matches.
3. Follow-up candidate: a validating webhook warning when a PF filter is
   unscoped, and/or refusing PFs that carry the node's default route.

## Open Questions

1. Should the `sriov_numvfs > 0` exclusion be overridable (a policy field)
   for operators who intentionally allocate a PF while tearing down VFs?
   Current answer: no — the invariant is cheap and the workaround (set
   `sriov_numvfs=0` first) is the operationally honest one.
2. Should `deviceType` also gain an `any` value? Deferred until a concrete
   need appears; two filters express it today.
3. Upstream `sriov-network-operator` integration: should
   `SriovNetworkNodePolicy` translation emit `deviceType: pf` for its
   PF-selecting policies? Tracked separately.

## References

- [opt-in-advertisement.md](opt-in-advertisement.md) — the advertisement
  model this design extends
- [SR-IOV Device Plugin](https://github.com/k8snetworkplumbingwg/sriov-network-device-plugin)
  — PF-capable selector semantics this driver aims to replace
- Current discovery: `pkg/devicestate/discovery.go`
- Filter matching: `pkg/controller/resourcepolicycontroller.go`
- Preparation: `pkg/devicestate/state.go`
