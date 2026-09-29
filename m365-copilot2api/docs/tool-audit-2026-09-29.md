# Gateway tool audit — 2026-09-29

## Scope and evidence

The release under review is `desktop-service-20260928`. Its initial deployed
binary was built from revision `ea7e55c` plus the uncommitted tool/affinity fixes
in this branch. The deployment-tree inventory covered all 21 files: 8 root
files, 8 frontend files, 1 native-panel data file, and 4 rollback files. Private
account, key and session contents are not included in this report or committed.

Source checks and regression execution covered tool extraction/JSON fidelity,
parallel limits and explicit serial mode, tool history and call IDs, all three
protocol adapters, streaming completion/error handling, account fallback and
session restoration, final Edit emission, startup resources and rollback.
Running the full test suite is not a proof that every possible bug is absent.

All new **live** model requests use `gpt-5.6-reasoning` with `max` effort. The
model catalog advertises that combination and the gateway's effort normalizer
accepts `max`. Model identity is the requested gateway route, not an independent
attestation of Microsoft's underlying model implementation.

## Repairs included

- Replace storage-order account fallback with the existing health-aware
  scheduler. Keep exclusions local and bounded; do not bypass cooldowns.
- Exclude common system/developer instructions from weak session similarity;
  do not infer affinity from a first-turn vocabulary match or a local-only
  record without a cloud conversation. Exact local continuation replays all
  history instead of sending an increment into an empty cloud conversation.
- Save the actual structured tools returned to clients. Router and native
  tool continuations can match their wire history rather than router prose.
- Preserve repeated independent Task/Agent/spawn/Read calls. Keep configured
  caps, explicit serial mode and shared-shell safety constraints.
- Parse inline fenced arguments with the existing quote-aware balanced parser;
  require a closing fence and preserve quoted braces, parentheses and paths.
- Enforce failed-Edit recovery at the final tool-response boundary, after
  dedupe overrides and all routing/repair branches. Until a successful Read of
  the same file was **issued after** the failure, another Edit is withheld.
  When allowed and schema-valid, return a non-mutating Read of that file;
  otherwise return an explicit recovery-required error (409 or a stream error).
- Do not inject a second Read when one is already selected. Do not treat an
  explicit `<tool_use_error>` Read result as a successful refresh. Preserve
  legal Edit retries after reading, including mixed tool batches.

The guard recognizes the declared `Edit`/`Read` contract. It is not a universal
policy for every arbitrarily named MCP patch tool. It cannot recover history
that the client has omitted. Generic repeated progress prose is **not** globally
filtered, and this audit does not claim the underlying model's instruction
following has been repaired.

## Verification

- `go test ./...`, `go vet ./...`, and `git diff --check` passed after the final
  emission-boundary changes. A source-structure test was updated to follow
  `emitToolCalls` instead of the old direct `writeToolResponse` call.
- New adversarial handler tests cover 3 protocols × 2 planning modes ×
  streaming/nonstreaming × Read allowed/disallowed = **24 combinations**.
  The stub model deliberately repeats a failed Edit; assertions inspect what
  actually reaches the client, not just a prompt telling the model to behave.
- Existing parser/spawn tests cover complex quoted arguments, 3 same-name
  calls, distinct IDs, namespace round-tripping, and stream/nonstream output.
- Affinity tests cover fair concurrent fallback, excluded/unhealthy candidates,
  healthy continuation, full-history replay, and tool-result round trips.
- Reverse-overlay tests against the old fallback selected the first account
  12/12 times and 40/40 times concurrently. The new scheduler distributes them.
- Initial live matrix: **20/22 passed**. Chat parallel nonstream and Responses
  serial nonstream hit the probe's 120-second client timeout. They are retained
  as failures, not silently replaced by later reruns. No live HTTP 429 appeared
  in that matrix. Passing parallel cases returned three actual structured calls
  with unchanged synthetic arguments, not tool-looking text.
- A ten-minute monitor observed frontend HTTP 200 throughout and five timeout
  diagnostic lines. Those lines are not necessarily five distinct requests.
- M365 independently reviewed the guard using the requested model/effort and
  identified duplicate recovery Reads; that issue was fixed with a regression.
- The separate orchestration service failed all five audit starts with 429 and
  produced no findings. It is not counted as an independent completed audit.
- Race instrumentation was unavailable: this Windows environment uses
  `CGO_ENABLED=0` and no C compiler was found. Concurrent functional tests ran;
  they do not substitute for a successful `-race` run.

## Reproducible live checks

Both scripts read credentials only from `M365_TEST_API_KEY` (or the named
`--api-key-env` variable). Do not put a real key in source control or reports.

```sh
python scripts/audit_tool_protocols.py --suite all --workers 2 --timeout 180 \
  --report ../_audit-sidecar/audit-20260929/live.json
python scripts/audit_edit_recovery.py --timeout 180 \
  --report ../_audit-sidecar/audit-20260929/edit-recovery.json
```

`audit_tool_protocols.py` inspects returned calls without executing them.
`audit_edit_recovery.py` executes only Read/Edit against a temporary
`audit.txt`, requires read → edit → verification, and deletes that temporary
fixture afterward. It never modifies the user's workspace. Both default to
loopback. Runtime outputs remain in ignored `_audit-sidecar/`; executable
backups remain in the deployment directory, not Git.

## Remaining audit findings and limits

- Legacy tenant identity derives a shortened Bearer-key prefix, while X-API-Key
  uses a different representation. Legacy User/SessionKey caches are not fully
  tenant-scoped. This is not a verified cross-user incident, but those paths
  must not be treated as a multi-tenant isolation boundary. A coordinated
  migration and dedicated cross-tenant tests remain required; this release
  does not silently delete or rewrite existing session stores.
- Previously inferred stale account hints can outlive rejected cloud-history
  hints in some legacy cache paths. The current weak-match guard prevents a
  mismatched account/cloud tuple there; a complete explicit/cache tuple audit
  remains a follow-up.
- A model/network request can still time out. A live timeout is not evidence
  that the tool parser rejected a valid call; correlate request traces before
  assigning cause. Provider limits and content refusals must remain respected.

Post-deployment measurements are recorded separately so the initial failed
measurements above remain visible. No production key, token, account address,
private prompt, or executable is committed.
