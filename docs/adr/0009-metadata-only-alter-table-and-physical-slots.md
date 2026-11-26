---
status: accepted
---

# Metadata-Only ALTER TABLE via Monotonic Physical Slots and Catalog Tombstones

fycsql v1 supports four transactional `ALTER TABLE` operations — `ADD COLUMN` (with optional `NOT NULL DEFAULT <const>`), `DROP COLUMN`, `RENAME TABLE`, and `RENAME COLUMN` — executed strictly as $O(1)$ metadata-only updates in `_fycsql_catalog` rather than physical table rewrites. Every table is addressed internally by an immutable numeric `TableID` and every column by a permanently monotonic, never-reused **Physical Slot** (capped at 1,024 slots per table lifetime). On read, older row records with `ColumnCount <= col.PhysicalSlot` transparently synthesize the column's immutable scalar `AddDefault` (or `NULL`), while dropped slots are tombstoned in the Catalog, skipped via ADR-0005's self-describing `VarintLenHeader`, and lazily reclaimed to 1-bit `NULL`s on subsequent row writes.

## Considered Options

- **Synchronous Physical Table Rewrite on `ADD`/`DROP COLUMN`**: Compacts dropped column payloads immediately and keeps logical and physical column ordinals identical, but rewriting multi-GB tables inside a single MVCC transaction breaches ADR-0001's 64MB Write Set cap (`ErrTxTooLarge`), stalls the Commit Gate, and requires complex rollback handling.
- **Reusing Freed Physical Slot Ordinals After `DROP COLUMN`**: Avoids a lifetime slot ceiling, but risks reading stale bytes from an older un-rewritten row as a newly added column (`ADD c → DROP c → ADD d`) unless every page in the table is scanned and scrubbed first.
- **Deferring All `ALTER TABLE` to v2**: Keeps DDL strictly to `CREATE`/`DROP`, but forces embedded application migrations to recreate and copy entire tables for routine additive schema changes.

## Consequences

- **Guaranteed $O(1)$ transactional DDL within Commit Gate & Write Set bounds**: All four `ALTER TABLE` operations execute inside standard `BEGIN ... COMMIT/ROLLBACK` transactions with RYOW Catalog overlay, acquire ADR-0001's exclusive Schema Barrier at commit, bump `SchemaVersion` (transparently re-binding/re-planning Prepared Statements per ADR-0006), and never block active prior-`SchemaVersion` read Snapshots.
- **Zero-rewrite `RENAME` & strict v1 guardrails**: Because Secondary Indexes reference `(TableID, []PhysicalSlot)` instead of names, `RENAME TABLE` and `RENAME COLUMN` touch zero table or index pages. `ALTER COLUMN TYPE`, `ADD/DROP PRIMARY KEY`, `ADD COLUMN ... PRIMARY KEY/UNIQUE`, `DROP COLUMN` on PK or indexed columns (caller must `DROP INDEX` first), and dropping the last visible column are explicitly rejected (`ErrConstraint`); `DEFAULT` is restricted to constant scalar literals (with ADR-0002 `INTEGER → REAL` widening).
- **Lazy space reclamation & `Verify()` compatibility**: Dropped-column payload bytes persist in untouched historical rows until those rows are next updated (when dropped slots write 1-bit `NULL` / 0 payload bytes), and full compaction waits for post-v1 `VACUUM`. `Verify()` (ADR-0005) validates `record.ColumnCount <= table.MaxPhysicalSlotCount`, skips tombstoned slots, and checks all visible non-NULL columns against their declared types. It rejects NULL in a visible NOT NULL column.
- **Historical rows and private DDL.** New and rewritten rows encode the full current physical slot count, including trailing explicit NULLs. A present NULL bitmap bit yields NULL even for a column with AddDefault. Point reads, projections, index builds and verification use the same snapshot-visible column mapping. Prepare after a private ALTER uses the local Catalog until commit or rollback (ADR-0006).
