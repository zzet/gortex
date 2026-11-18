# Mixed background timing qualification

`check-budgets edit-qualify --report raw.json --statuses pins.json --out qualification.json` writes a new, versioned overlay under policy `shared-mixed-background-b-only-v1`. It never changes the report or a producer outcome, original error, timing, or whole-run completion flag. It does not certify aggregate acceptance or the three same-build runs.

The schema-1 pin file contains `report_sha256` and `statuses`: each entry has `ordinal`, `path`, and `sha256`. A status path names the unchanged per-edit JSONL raw-evidence artifact. Its byte count must equal the report’s captured evidence count. The checker consumes only its final status RPC, refusing malformed frames, a final refusal, or contradictory content/structured bodies. Relative paths resolve beside the pin file. Both artifact and decoded status hashes are retained in the output.

Qualification requires one shared physical worktree/file, all committed-ACK/read/oracle/freshness/capture predicates, exact original receipt/mutation/RPC/generation ownership, and separately recomputed measured A/P partitions within 5%. Every physical phase remains measured. Only the exact owned `preexisting_path_work` background refusal may leave B unavailable; unknown reasons, partial P, native no-ops, successor windows, foreign owners, failed reads, stale tiers, capture loss, and other aggregate failures are rejected. Existing qualified outcomes keep their independent validation.

The output preserves raw report identity and errors, original diagnostic outcomes, and status hashes. Missing B is null with an explicit mixed-window reason. The accepted attribution follow-up is #1432, unscheduled after v0.65.0 and nonblocking for v0.64.12 and v0.65.0.
