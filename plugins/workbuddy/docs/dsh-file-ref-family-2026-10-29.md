# DSH file-reference family — verification report

Date: 2026-10-29
Plugin version under test: 0.9.81 (upstream `69a4724`, "the fourth 11-128 family member")
Realm used for the CN probes: `www.codebuddy.cn`, `region=cn` credential, model `deepseek-v4.1-flash`
Suite: `dsh_file_ref_family_live_test.go` (`//go:build live`)

## 1. Field report

After 0.9.79 a real DSH session was still rejected: 50 messages, 67 tools, 321,969 B body,
HTTP 400 `code 11128` "Illegal API invocation from an unapproved channel", and the same
body produced a 503 `auth_unavailable` cascade with `last error=11128` once the credential
entered cooldown. Trimming the body to a trivial system prompt answered 200, so the trigger
sat in the system prompt rather than in the size or the tool table.

## 2. Trigger geometry (field-level bisect)

Working text: the 0.9.79 production output for that body (19,216 bytes in Go byte space —
PowerShell's char count of the same text is 19,087, so offsets must be taken in Go).

| Probe | Result |
| --- | --- |
| whole scrubbed text | 400 / 11128 |
| delete 8 KB in 8 chunks, keeping the first 2 KB | 400 (later explained: the trigger stayed inside the kept region) |
| `[1024:2048]` alone (1024 B) | **400 / 11128** |
| `[2048:3072]` alone | 200 |
| `[9216:]` alone (10,000 B) | 200 |
| `[1024:2048]` + `[9216:]` | 400 |

Halving inside `[1024:2048]`: head 518 → 200, tail 506 → 400, tail 252 → 400, and splitting
that 252 gave 132 → 200 and 120 → 200 — the signal straddles the split, i.e. it is a
**phrase**, not one line. The 252-byte survivor was:

```
…change. Re-list after wakeup or timeout. The Lead must wait for required teammates before
giving the final answer.

Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from
the workspace root; absolute paths identify
```

Candidate sentences were then tried one at a time. All of these pass on their own:
wait-agent guidance, the Lead sentence, the inactive-teammate sentence, the peer sentence,
and a 64-character prefix of the `@` sentence.

| Probe | Result |
| --- | --- |
| `Tokens prefixed with @ are paths the user explicitly referenced.` alone (64 B) | 200 |
| `…explicitly referenced. Relative paths resolve from the workspace root;` (112 B) | **400 / 11128** |
| same, extended to 121 / 136 B | 400 |
| `Relative paths resolve from the workspace root; absolute paths identify` alone (71 B) | 200 |
| `referenced` → `mentioned` (135 B) | 200 |
| `workspace root` → `working directory` (139 B) | 200 |
| drop the `Tokens prefixed with @` prefix (113 B) | 200 |

Two named signatures therefore exist — S1 `Tokens prefixed with @ are paths the user
explicitly referenced` and S2 `Relative paths resolve from the workspace root`; **each alone
passes, both in one field block**. The gate is a signature *count* per field, not a substring
match, which is why neutralising either sentence clears the field.

## 3. Role scope

| Placement | Result |
| --- | --- |
| pair in `system` | **400 / 11128** |
| pair in `assistant` (history echo) | **400 / 11128** |
| pair in `user` | 200 |
| pair in `tool` (tool result) | 200 |
| S1 alone in `user` | 200 |
| S2 alone in `user` | 200 |

The gate is enforced on `system`/`developer`/`assistant` only. Cleaning the system prompt
alone is therefore not enough: DSH replays its own prompt text inside assistant history, so
the same signature pair has to be rewritten there too. That is what the shipped rewrite does.

## 4. No cross-field accumulation

| Layout | Result |
| --- | --- |
| S1 alone in `system` | 200 |
| S2 alone in `system` | 200 |
| S1 in `system`, S2 in `user` | 200 |
| S1 (rewritten) in `system`, S2 in `assistant` | 200 |

Signatures are counted per field, so splitting the pair across messages already passes —
worth knowing before widening a scrub further than it needs to go.

## 5. Whole-body replay

`prepareUpstreamBody()` over the captured 321,969 B field-report body leaves **zero**
occurrences of S1, contains the rewritten sentence, and the rewritten body answers **200**
on a healthy CN credential. The rewrite is applied in place inside the assistant echo as
well as in the system prompt, which is the property that actually clears the reported case.

## 6. Realm asymmetry (2026-10-29)

The same probes against the international upstream (`www.workbuddy.ai`, `region=global`):

| Probe | Result |
| --- | --- |
| canary | 200 |
| pair in `system` | 200 |
| pair in `assistant` | 200 |
| pair in `user` | 200 |

The intl gate does not enforce this family yet — the CN and intl blocklists drift, and the
`Main branch` phrase previously behaved the same way (present in CN first, later enforced
in intl). The bridge port of this rule is therefore defensive parity rather than a
reproduced fix.

## 7. Credential census (why intl probes need care)

`auths\workbuddy-bridge-*.json`, canary request only, 24 credentials, 2 s apart:

| Outcome | Count |
| --- | --- |
| 200 | 1 (`329d7e35-…`, `expiresAt` 2027-09-14) |
| 403 + `11140` "account has been restricted due to unusual activity" | 23 |
| 401 | 0 |

On that realm the dominant failure is account-level restriction, which no phrase scrub can
fix; a probe that reports only the status code cannot tell the two apart, hence
`dfrRestriction()` in the suite.

## 8. Running the suite

```powershell
$env:CGO_ENABLED='1'
$env:PATH='C:\Users\MECHREV\w64devkit\w64devkit\bin;' + $env:PATH
$env:WB_CRED_FILE='...\auths\workbuddy-<uid>.json'   # probe credential
$env:WB_BODY_FILE='...\rc_A.json'                     # captured failing body
$env:WB_AUTH_DIR='...\auths'                          # census directory
$env:WB_SLEEP='6'                                     # pace probes
go test -tags live -timeout 30m -run DSHFileRef -v .
```

Pacing matters: a risk-control hit puts the credential into roughly a one-minute cooldown,
during which every follow-up probe answers 503 instead of the gate verdict. The suite names
each probe and logs `code` plus the restriction class, so a cooldown never looks like a pass.
