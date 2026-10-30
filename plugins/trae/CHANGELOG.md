# Changelog

## 0.12.82

### The auth.parse response carries the operator fields (issue #68)

Same fix as workbuddy 0.9.82: the host rebuilds its in-memory Auth record
from the auth.parse response and does not re-read proxy_url / prefix /
priority from the file, so the panel proxy reset to "none" after restarts
and after plugin-side rewrites. `applyHostOwnedAuthFields` (new,
`host_owned_fields.go`) re-injects the #25 whitelist into the AuthData; the
physical document arrives base64-encoded on the wire and is decoded first
(the decoded storage payload does not carry the operator keys). Unit +
end-to-end handleParseAuth tests pin the behaviour.


## 0.12.81

### A claim that answered code:0 is never re-sent the same day (magpie trae 0.2.22 discipline)

Absorbed from magpie-community/plugins `4ac4766` (trae 0.2.22, 2026-10-25, #333/#361). Magpie's measured premise: a claim's own code 0 is not proof the day is in — Trae answers code:0 for a claim that registered nothing, and code:0 is also the idempotent echo for a day already checked. Once a claim has been ACCEPTED, what the status read-back says is the day's answer, and nothing is claimed again for that account that day.

- Our old behavior treated "code:0 + read-back unconfirmed" (and "read-back failed") as retryable-today — the 10m-backoff ladder would **claim again**, which is exactly the multiply-claim case magpie closed: same-day multi-claims are the "unusual request patterns" family upstream risk control flags (the workbuddy plugin's own 403+11140 field evidence, 0.9.73).
- New `upstream/claim_once.go`: a per-account, per-UTC+8-metering-day accepted-claim registry + the pure `ClaimGate(checkedIn, enable, sentToday)` decision. Both the scheduler loop and the manual panel path now gate through it; the claim branch records the acceptance (`NoteClaimSent`) the moment code:0 arrives.
- Scheduler: the two post-claim read-back cases no longer count into the same-day retry ladder (no `rateLimited++`); a marked account's later rounds are status reads only, logged as such.
- Manual path: re-pressing after an accepted claim shows `claim_skipped` — "claim already answered code:0 today — status re-read only, not claimed again today"; the `claim_unconfirmed` honesty marker stays (the panel never fakes success).
- What deliberately does NOT change (our evidence-based value-add over the reference): 9074 refusals keep the same-day retry ladder + device rotation (v0.12.73) — a 9074 means the server REFUSED the claim, so no claim happened; magpie's own capture (2026-10-23: same account and headers taken on a device id Trae had never seen while the account's own id answered 9074) is that behavior's evidence base. Client-profile reading (magpie 0.2.22 fix #1) has no equivalent surface here — this plugin maintains its own sign-in device identities (0.12.79 model) and never reads local Trae client profiles.
- Day key: UTC+8 metering day (Trae CN's reset boundary). In-memory only: a restart re-reads the upstream status — the authoritative day answer — before any claim, so the worst case after a restart is a status read, never a blind re-claim (pinned by `TestClaimOnceRegistryRestart`).
- Tests: `upstream/claim_once_test.go` — registry day scoping, the UTC+8-midnight boundary, per-account isolation, restart semantics, and the ClaimGate truth table. Full trae suite green under `-race` (all 7 packages).
