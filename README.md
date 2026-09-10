# motor-almacenamiento

An embedded key-value storage engine in Go, standard library only. A B+tree over 4 KiB
pages, with a write-ahead log and crash recovery.

**The guarantee, and everything else exists to hold it up:** if `Put` returns `nil`, that
record survives any later crash. Not because it is in its final place — it probably isn't
yet — but because it is in the log, and that log has already been through `fsync`.

## What it does

The tree is the easy half; it's a data structures exercise. What the project is actually
about is **proving the guarantee**:

- **A 500-point crash sweep.** For each of the first 500 writes, the engine runs against a
  fake disk that crashes on exactly that write, then discards part of the unsynced queue,
  reorders what survives, and tears one write at a sector boundary. On reopen: the six tree
  invariants must hold, every acknowledged key must be there with its value, and no key that
  was never attempted may appear. Every point carries its seed, so any failure is
  reproducible byte for byte.
- **A real `kill -9`**, in a child process, because killing a process does not flush the
  operating system's cache — it tests the process boundary, not the disk boundary. Both are
  needed, and neither sees the other's failures.
- **Property testing** against a reference `map`: thousands of random operations, compared
  after every one.
- **A correctness argument.** The engine performs exactly five `fsync` calls on its normal
  path. For each one: what does recovery see if the crash lands just before, and just after?
  Ten cases, all ten now covered by fault injection, each mapped to the tests that exercise
  it.

Every bug found along the way is written up in `BUGS.md`: what caused it, how it was caught,
and the seed that reproduces it.

## Quickstart

```go
import motor "github.com/jsav2003/motor-almacenamiento"

db, err := motor.Open("path/to/db")   // the directory is created if missing
if err != nil {
    return err
}
defer db.Close()

if err := db.Put([]byte("key"), []byte("value")); err != nil {
    return err   // only here; if it returns nil, the record is already durable
}

v, err := db.Get([]byte("key"))       // ErrNotFound if absent
if err != nil {
    return err
}

err = db.Scan(nil, nil, func(k, v []byte) bool {
    fmt.Printf("%s = %s\n", k, v)
    return true                        // return false to stop the scan
})
```

## API

| Function | Notes |
|---|---|
| `Open(path) (*DB, error)` | A database is a directory: `datos.db` holds the tree, `datos.wal.N` the log for generation N. Opening **is** recovering — there is no separate "normal open" path, so the recovery code runs every time and is therefore always tested. |
| `(*DB) Put(key, value) error` | Returns `nil` only once the commit record for its group is synced to the log. |
| `(*DB) Get(key) ([]byte, error)` | The returned value is **a copy** and may be retained. `ErrNotFound` when the key is absent. |
| `(*DB) Scan(start, end, fn) error` | Half-open range `[start, end)` in key order; `nil` bounds mean unbounded. The `k` and `v` handed to `fn` are **only valid for the duration of the call** — copy them if you keep them. Returning `false` stops the scan, and that is not an error. |
| `(*DB) Validate() error` | Checks the six tree invariants. Expensive; meant for debug mode and for the crash harness, where it is criterion (a) at every crash point. |
| `(*DB) Close() error` | Runs a full checkpoint and closes the files. Closing without it loses nothing, it only makes the next open slower. Closing twice is not an error. |

Errors: `ErrNotFound`, `ErrEntryTooLarge`, `ErrKeyTooLarge`, `ErrCerrada`. They are
re-exported from the root package because the `internal/` ones are not importable from
outside the module — without that, a caller could not tell a missing key from a disk
failure.

Size limits: keys up to 512 bytes, and `key + value + 6` may not exceed 1000. The number is
derived from the fill target, not from convenience: four cells of the maximum size have to
fit in one page for a full leaf to always split into two reasonable halves.

## Tests

```sh
go test ./...              # everything: 12 packages, about half a minute
go test -short ./...       # skips the four expensive ones
go test -race ./...        # needs cgo
```

`-short` skips exactly the four tests this project exists for: the 500-point crash sweep, the
100,000-key tree, the two property runs and the `kill -9`. CI runs **without** `-short`.

```sh
go test -run TestQuinientosPuntosDeCaida -v .   # the fault injection table
go test -run TestCaidaYReapertura -v .          # kill -9, in a real child process
go test -fuzz FuzzArbol ./internal/tree         # one of the five fuzz targets
```

## Known limitations

Written down rather than left to be discovered:

- **There is no `Delete`.** It has no phase assigned, and a public method that returns "not
  implemented" is worse than one that isn't there, because it compiles in the caller's code.
  The consequence is bigger than the missing method: nothing is ever freed, so **everything
  this engine demonstrates is about a tree that only grows.**
- **CI has never run.** The workflow is written and pushed — Linux + Windows matrix, full
  suite, `-race` on Linux — but the account is locked over billing and both jobs die in two
  seconds without executing a step. A workflow that never ran is exactly as trustworthy as a
  test that never ran.
- **The race detector has never run either.** It needs cgo, and neither the development
  machine nor the WSL2 Ubuntu has a C compiler. A single writer thread and not one goroutine
  mean confirmation is what's expected — but "expected" is not "checked".
- **Directory `fsync` is a no-op on Windows**, where all development happened. That is not an
  implementation choice: Windows does not hand out a directory handle that accepts
  `FlushFileBuffers`. The POSIX path has only been exercised by hand, on ext4 under WSL2.
- **The fake disk does not corrupt already-written sectors.** It discards, reorders and tears
  *unsynced* writes. A bit flipped in a sector that was already on the platter is a different
  failure class, covered by the single-bit corruption tests instead.
- **The 500-point sweep runs with volatile directory entries turned off.** Crashing between
  the creation of the new log generation and the directory `fsync` is modelled and covered —
  but by a separate test with its own seeds, not by the 500 rows.
- **The pager cache is unbounded**, so the engine never evicts a dirty page whose log record
  is still unsynced. The write-ahead eviction rule is tested by hand, not by the sweep.
- **Two design decisions are still open**, tracked in `docs/DEUDA-DISENO.md` as D9 and D11.
  D11 is a contradiction in the design with itself.

## Not implemented

Permanent exclusions, not a roadmap. Each one is a different project:

- SQL: no parser, no query planner, no relational algebra.
- Concurrency: a single writer thread, no multi-operation transactions. `DB` is **not** safe
  for concurrent use.
- Network: no client, no server. This is an embedded library.
- Secondary indexes: the only order is the primary key's.
- Compression, replication and encryption.
- Overflow pages: a key/value pair has to fit comfortably in a 4096-byte page.
- Competitive performance: it does not try to measure itself against real engines.

## Documents

The design documents are **in Spanish**. They are the working record of the project:

| File | What it is |
|---|---|
| [`DESIGN.md`](DESIGN.md) | The specification. Written **before** the code, and not edited on the fly. |
| [`NO-GOALS.md`](NO-GOALS.md) | The limits, explicit and permanent. |
| [`TESTING.md`](TESTING.md) | What the suite proves and what it does not: the fault injection table and the correctness argument. |
| [`BUGS.md`](BUGS.md) | Every bug by phase: what caused it, how it was caught, with the seed that reproduces it. |
| [`docs/DEUDA-DISENO.md`](docs/DEUDA-DISENO.md) | The twelve design decisions and their status. |
| [`docs/REVIEW-01.md`](docs/REVIEW-01.md) | An adversarial review of the v1 design, done before any code was written. AI-assisted, identified as such. |

If you only read two: `TESTING.md` for what is proven and how, and `BUGS.md` for what broke
along the way. The second says more about a project like this than the code does.

## License

MIT. See [`LICENSE`](LICENSE).
