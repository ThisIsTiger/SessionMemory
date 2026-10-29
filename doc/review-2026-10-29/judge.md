# Judge verdict: A01, A02, A03, A04, A11, A12

Method: ran `run_audit.py` (18 failures, 16 distinct tests, no build errors; all six findings reproduce). Read the code for each root cause. Prototyped each fix in a scratch copy of the repo, not in the real tree. In the copy all six overlay tests pass, and the rest of the suite (`-tags dev,failpoint`, default packages) shows no regression. Only unrelated audit tests still fail there: A05-A10, A13, A15, A16. The prototype diff is at `/private/tmp/claude-501/-Users-tiger-Documents-tiger-SessionMemory/45028a59-cbcb-4a15-9255-e1412e35fb49/scratchpad/judge-prototype.patch` (about 130 added lines in 7 files). Non-test source in the real repo is untouched.

| ID | Verdict | Root cause confirmed | Real-user likelihood | Impact |
|---|---|---|---|---|
| A01 | REAL-NOW | Yes. `Recover` -> `undoInstall` blindly rewrites `Backed` files and deletes `Created` files. The journal never records the expected hash of the written files, and `GuardRecovery` only checks locks and process state. | Low. Needs a crash or kill inside the install window, then Codex continuing the thread and closing again before the next session-memory run. | Silent loss of the user's own conversation. Create case deletes the whole file. |
| A02 | REAL-NOW (cheap, shares code with A14) | Yes. `jsonl.NewCursor(path)` re-opens by path and hashes whatever is there. Nothing ties it to the bytes just stored. | Very low for append-only Codex files. | Permanent silent omission: the cursor certifies the new file, so it is never re-read. |
| A03 | REAL-NOW (same file as A01) | Yes. `Writer.Plan` and `Apply` only look at snapshot paths, but `runInstall` adopts through `ad.Capture(Cursor{})`, which scans every rollout of the thread (`threadFiles`). | Low. Needs an unsynced second `rollout-...-<thread>_<uuid>.jsonl`. A diverged main file is already refused. | Local-only progress is silently absorbed into the cursors. No revision contains it, so it never syncs. No local data loss. |
| A04 | REAL-NOW | Yes. `CatchUp` only visits accounts present in the behind file with value true. A new account is never "unknown -> behind". | High. Adding a backup token later is the normal path for the feature just shipped. | The backup is silently incomplete, so the "two full copies" promise is false. |
| A11 | REAL-LATER | Yes. The "never create a second vault" check in `joinOrCreateVault` only runs when `m != nil` (Weiyun). The Folder branch goes straight to `init`. | Very low. Needs Folder transport, `setup`, and a removed key. The user uses Weiyun. | Wrong local vault identity, which blocks re-joining until it is cleaned up. |
| A12 | REAL-LATER | Yes. `MkdirAll` runs before the `planOnly` branch, and the `Stat` check then demands the directory exist. | Low. Needs `--plan` on a WorkBuddy task project with no mapping. | An empty directory left behind. Cosmetic. |

## A01 design
- `internal/store/journal.go`: add `InstallData.Planned []agent.PlannedWrite` (final path and sha256 of every file the install will write).
- `internal/source/codex/writer.go`, `Apply`: set `Planned: p.Writes` in the `InstallData` first stored at `StStaged`, so it is present from `StBackedUp` onward.
- `internal/store/tx.go`: add `checkInstallUntouched(d)` and call it only in the `Recover` Undo branch, after the guard and before `undoInstall`. Do not call it from `RollbackTx`, because live rollback after a failed `Verify` must still work.
- Invariant: each planned file must be the written bytes (sha equals `pw.SHA256`), or the backup bytes (not yet written, or already rolled back), or absent when it is only `Created`. A backed-up file that is missing also counts as changed. Anything else returns `RecoveryNeeded`.
- Safe failure: the journal, backups and files are kept and nothing is overwritten. Old journals with no `Planned` fall back to the old behaviour. WorkBuddy is unaffected; it has its own checks in `UndoRegistry`.
- Residual: `Recover` errors make `openRT` fail for every command, which wedges the tool until the user acts. Make the error name the journal path. Follow-up (REAL-LATER): a `recover --abandon <tx>` command that archives the journal and keeps files and backups.
- Test: promote `TestAuditRecoveryPreservesPostCrashContinuation/{create,append}` into `internal/sync`. Add a positive case where the file is untouched after the crash and recovery still rolls back. The existing install crash matrix must stay green, and it did in the copy.

## A02 + A14 design (`internal/streamcap/streamcap.go`)
- Factor `blobChunks(data)` out of `CaptureBlob`, and `verifyRange(src, from, segs)` out of `VerifySegments` (which becomes `verifyRange(src, 0, segs)`).
- A02: in `CaptureStream`, plain files only, remember `nOld := len(segs)` before the loop. After `NewCursor` succeeds, run `verifyRange` over `segs[nOld:]`. On mismatch return the exported `ErrChangedDuringCapture`. No cursor is persisted, and orphan objects are harmless because objects are immutable. The next run sees the rewrite through `PrefixMatches` and bumps the generation.
- zstd needs nothing: its cursor is offset-only and the next capture re-verifies the whole stream.
- A14: in `VerifyArtifacts`, when `deep` is set, a blob is bad if `SegmentsDigest(blobChunks(data)) != st.Digest`. Keep the non-deep behaviour unchanged.
- Cost: one extra read of only the newly captured range.
- Tests: promote `TestAuditReplacementDuringCaptureIsNotLost` and `TestAuditDeepVerifyDetectsEqualLengthBlobChange`. Tighten the first so the error is `ErrChangedDuringCapture` and not just any error.

## A03 design (`internal/source/codex/writer.go`)
- Add `extraThreadFiles(home, nativeID, paths)`: the thread's rollout files from `threadFiles` whose logical name is not in the snapshot's paths.
- `Plan` returns `NeedsAction` ("checkpoint them first") when any exist, so `--plan` shows it. `Apply` rechecks after the baseline check under the vendor lock and returns `BaselineChanged`.
- Do NOT put this generically in `engine/runInstall`. I tried it: WorkBuddy logical paths contain the cwd-encoded directory, so three WorkBuddy tests fail. Keep it Codex-scoped.
- Trade-off: a locally archived copy (`archived_sessions/...`) of the thread is also refused. That is correct, since the install would otherwise create a duplicate in `sessions/`.
- Test: promote `TestAuditRestoreDoesNotAdoptUnpublishedExtraRollout`. Also assert the extra file is untouched and no binding is created.

## A04 design (`internal/transport/multi.go`)
- Change the meaning of the behind file: a label present means known (true = behind, false = level); absent means never seen, so treated as behind. `setBehind(false)` stores false instead of deleting the entry.
- `CatchUp`: return early if `StateFile == ""`. If any account is unknown, first `each(Pull)`; if any pull fails, skip the unknown accounts this round. Otherwise, with a partial local copy, we would mark the backup "level" with history missing. Then `Push` the unknown or behind accounts and mark them level.
- Cost: one one-off Push per account after upgrade.
- The call site `syncOnce` -> `CatchUp` needs no change.
- Test: promote `TestAuditMultiMirrorCatchesUpNewBackup` into `multi_test.go`. Add a case with a fresh, empty local copy where the other account holds history: the backup must receive it only after the pull. `multi_test.go` was green in the copy.
- Related but separate: A10 (key file) uses the same state, so design it together later.

## A11 design (`cmd/session-memory/cmd_onboard.go`, `joinOrCreateVault`)
- Compute one `vaultFile` path: the Weiyun cache `public/vault.json` after `m.Pull`, or `<ExpandPath(folder)>/public/vault.json` for Folder.
- If it exists and no key was found, return the existing "key removed, run `key publish`" error before `runSelf init`. The text changed from "Weiyun" to "cloud folder" in the prototype.
- Test: promote `TestAuditSetupMissingFolderKeyDoesNotCreateForeignIdentity`.

## A12 design (`cmd/session-memory/cmd_project.go`)
- Change to `if autoTask && !planOnly { MkdirAll }`, and relax the `Stat` check when `autoTask && planOnly`.
- The plan will then carry a `project_directory_missing` gap. It is honest, and the real run still creates the folder.
- Weakness in the overlay test: with the default config the plan stops earlier because the WorkBuddy writer is disabled, so the test never proves the plan can succeed. The formal test needs `allow_registry_write` plus a local WorkBuddy home with a session. I could not build a full fixture within budget; in my attempt the plan failed with "open WorkBuddy and create a local session". Keep the stat-based assertion, and add a second assertion that a non-plan run does create the folder.
- Optional follow-up: move the real-run `MkdirAll` to after the install preconditions. The engine already has an `autoCreate` path in `restoreWorkspaceNotes`.

## Interactions and ordering
1. A01 and A03 both edit `codex/writer.go`: A01 adds `Planned` in `Apply`, A03 adds `extraThreadFiles` in `Plan` and `Apply`. Land them in one commit.
2. A02 and A14 share the `blobChunks` and `verifyRange` refactor. Land them together.
3. A04 is independent. A11 and A12 are CLI-only and independent.
4. Residual risks:
   - After A01, a refused recovery blocks all commands until a manual step (follow-up above).
   - The A02 error aborts that session's capture for one round only.
   - The A03 NeedsAction message should tell the user to run checkpoint or adopt.
