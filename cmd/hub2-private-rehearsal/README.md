# Hub 2 private staging importer

Builds the private SQL staging database of cosmoshub-2 from sealed financial
components. It writes only to a loopback database named
`legacy_replay_rehearsal_hub2_SUFFIX` as the confined importer role. Ports
15432 and 5432 are refused. It has no network client and no production path.
`ExportApprovedProjection` in `legacy/hub2promotion` reads the result.

## What a manifest lists

A contiguous run of sealed components starting at height 1. Each is 200,000
heights; the last one of the chain ends at the canonical export, 2,902,000.
Any prefix is valid: 7 components (1,400,000), 10 (2,000,000), all 15.

- The restart payout is the export at 2,902,000. A manifest must carry it
  exactly when it reaches that height and may not carry it earlier. Its rows
  are written on height 2,902,000 with phase 4 and nowhere else. The ledger
  key cannot hold them twice and the final check compares their count with
  the sealed export.
- Every earlier zero-height export is pinned as validation evidence. A row
  from one is refused. It is never income.
- Heights 2,902,001 and 2,902,002 are the historical tail. They are stored
  as headers and transactions with a tail receipt and no tax row.

## Modes

| mode | does |
|------|------|
| `prepare` | creates the named database, the legacy schema and the importer role |
| `extend-manifest` | writes a new manifest listing one more sealed component |
| `add` | imports every listed component that is not completely stored |
| `import` | the same, but refuses a database that already holds rows |
| `fingerprint` | hashes all 14 legacy tables in key order, optionally `-through` a height |
| `snapshot`, `refuse-retry` | the 1.4M rehearsal's retry proof, unchanged |

`-database` and `-port` select the staging database. `-parallel` (1 to 4)
imports that many components at once in `add` and `import`.

## Adding the next sealed component

```sh
hub2-private-rehearsal -mode extend-manifest -manifest PREVIOUS.json \
  -component /abs/path/to/NNNNNNN-ready-continuation/NNNNNNN -out NEXT.json
hub2-private-rehearsal -mode add -database NAME -port PORT \
  -manifest NEXT.json -out NEW_RECEIPT_DIR
```

`extend-manifest` only reads. It requires `final-component-acceptance.json`
to say the component is accepted with zero held heights, to start at the
height after the manifest's last, and, when the seal names the previous seal,
that seal to be pinned already. It pins the records, status, differences,
cache list, both proofs and the export records, and the binary that will run
the import. With the last component add
`-tail-cache DIR -tail-cache-manifest SHA256_LIST` for the two tail heights.

`add` rehashes every pinned file before and after, loads the genuine genesis
once (later runs compare the stored copy with the file), then for each
component:

- completely stored: checked by its header, tax row, transaction and event
  counts against the seal, and by the source digest in every height's
  receipt. Not read again.
- absent or partly stored (an interrupted run): streamed through the
  comparing writer, which inserts missing heights and refuses any stored
  height that differs. Nothing is updated or deleted.

After the components it checks each seam (the first block of a new component
names the stored last block of the one before), no gap or duplicate header,
transaction counts, payout placement and count, and no tax row after the
export. The receipt directory must be new.

## Why one at a time equals one run

The rows of a height depend on its component only. The receipt stored with
each height names the component's range, records digest, status digest and
cache list digest, the decoder commit and the genesis digest, and nothing
about the manifest that listed it. `fingerprint` of a database built one
component at a time equals that of a database built in one run; the only
column left out is `genesis_files.loaded_at`, the wall clock of the insert.

## Tests

`go test ./cmd/hub2-private-rehearsal` covers layouts and targets.
`TestActualRestartPayoutWiring` (`HUB2_STAGING_PAYOUT_TEST=yes` and the
`HUB2_STAGING_TEST_*` variables) runs the real add path on two real sealed
components with the canonical height lowered to the second one's end, in a
throwaway database, to exercise the payout wiring before the real 2,902,000
component exists. That database is a fixture, never staging.
