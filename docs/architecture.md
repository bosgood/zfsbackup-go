# zfsbackup-go architecture

Diagrams of the code on branch `clean-dry-run`. Each diagram names the function that does the work.

1. [Packages](#1-packages)
2. [Commands and entry points](#2-commands-and-entry-points)
3. [send: control flow](#3-send-control-flow)
4. [send: smart planning](#4-send-smart-planning)
5. [send: data pipeline](#5-send-data-pipeline)
6. [One volume: writer layers](#6-one-volume-writer-layers)
7. [receive: data pipeline](#7-receive-data-pipeline)
8. [receive --auto: chain walk](#8-receive---auto-chain-walk)
9. [clean](#9-clean)
10. [Storage layout](#10-storage-layout)
11. [plan: simulator](#11-plan-simulator)

## 1. Packages

```mermaid
flowchart TD
    main["main.go"] --> cmd

    subgraph cmd["cmd/ — cobra CLI"]
        root["root.go<br/>global flags, keyrings,<br/>working dir, exit codes"]
        sendC["send.go"]
        recvC["receive.go"]
        listC["list.go"]
        cleanC["clean.go"]
        planC["plan.go"]
    end

    subgraph backup["backup/ — orchestration"]
        bk["backup.go<br/>Backup, ProcessSmartOptions"]
        rs["restore.go<br/>Receive, AutoRestore"]
        cl["clean.go<br/>Clean"]
        ls["list.go<br/>List, linkManifests"]
        pl["plan*.go<br/>planner, Scenario, Schedule, checks"]
        sy["sync.go<br/>prepareBackend, manifest cache"]
    end

    subgraph files["files/ — data model"]
        ji["jobinfo.go<br/>JobInfo = manifest,<br/>object names"]
        vi["volumeinfo.go<br/>VolumeInfo: compress,<br/>pgp, hash, file or pipe"]
        mf["manifest.go<br/>ReadManifest (1 GiB cap)"]
        at["atomic.go<br/>WriteFileAtomic"]
    end

    subgraph backends["backends/ — Backend interface"]
        be["Init / Upload / List / Download<br/>PreDownload / Delete / Close"]
    end

    cmd --> backup
    cmd --> pgp["pgp/<br/>keyrings"]
    backup --> files
    backup --> backends
    backup --> zfs["zfs/<br/>runs zfs list / get / send / receive"]
    files --> pgp
    zfs --> zfsbin[["zfs binary"]]
    be --> s3[("s3://")]
    be --> gs[("gs://")]
    be --> az[("azure://")]
    be --> b2[("b2://")]
    be --> ssh[("ssh://")]
    be --> file[("file://")]
    be --> del[("delete://<br/>removes temp files")]

    config["config/ + log/<br/>globals: WorkingDir,<br/>BackupTempdir, AppLogger"]
    cmd -.-> config
    backup -.-> config
```

`internal/fakezfs` is a fake `zfs` binary for the e2e tests. Production code does not use it.

## 2. Commands and entry points

| Command          | Entry point                                   | Reads                               | Writes                                               |
| ---------------- | --------------------------------------------- | ----------------------------------- | ---------------------------------------------------- |
| `send`           | `backup.Backup` (`backup/backup.go:401`)      | zfs, manifests at every destination | volumes + manifest at every destination, local cache |
| `receive`        | `backup.Receive` (`backup/restore.go:253`)    | one manifest, its volumes           | `zfs receive`                                        |
| `receive --auto` | `backup.AutoRestore` (`backup/restore.go:54`) | all manifests of the dataset        | many `Receive` calls                                 |
| `list`           | `backup.List` (`backup/list.go:44`)           | manifests at one destination        | stdout                                               |
| `clean`          | `backup.Clean` (`backup/clean.go:176`)        | all objects at one destination      | deletes objects, local cache files                   |
| `plan`           | `cmd.runPlan` (`cmd/plan.go:143`)             | zfs or files, manifests             | stdout only                                          |

Exit codes (`cmd/root.go:94`): `0` = success or nothing new; `2` = plan checks failed or last backup newer than pool; `1` = other plan error; `255` = any other error.

## 3. send: control flow

```mermaid
flowchart TD
    A["zfsbackup send vol[@snap] uri1,uri2"] --> V["validateSendFlags<br/>loadSendKeys, ValidateSendFlags"]
    V --> S{"smart option?<br/>--full / --incremental /<br/>--fullIfOlderThan"}
    S -- no --> M["user names snapshots<br/>zfs get creation"]
    S -- yes --> P["ProcessSmartOptions<br/>(see diagram 4)"]
    P -- noop --> NOOP(["ErrNoOp: exit 0"])
    P -- full / incremental --> B
    M --> B["Backup()"]

    B --> DR{"--dry-run?"}
    DR -- yes --> RDR(["reportDryRun: log only"])
    DR -- no --> PB["prepareBackend + getCacheDir<br/>for each destination"]
    PB --> LK{"lock<br/>workdir/locks/md5(vol).lck"}
    LK -- busy --> LKE(["error: another send or clean runs"])
    LK -- ok --> GUID["RecordKeyFingerprints<br/>recordSnapshotGUIDs"]
    GUID --> RE{"refuseExistingSet:<br/>manifest already at…"}
    RE -- "all destinations" --> REE(["error: refuse to overwrite"])
    RE -- "some, no --resume" --> REE2(["error: run with --resume"])
    RE -- "some, with --resume<br/>or CompletePartial" --> CM["copyManifest<br/>verify each volume size + SHA-256<br/>then upload manifest"] --> DONE(["done, nothing sent"])
    RE -- none --> RS{"--resume?"}
    RS -- yes --> TR["tryResume<br/>keep volumes cached at every<br/>destination; skip their bytes"]
    RS -- no --> DP["discardPartialManifests"]
    TR -- "only the manifest was missing" --> DONE
    TR --> VS["validateSnapShotExists<br/>base + incremental"]
    DP --> VS
    VS --> PIPE["run pipeline<br/>(see diagram 5)"]
    PIPE --> OUT(["print totals"])
```

## 4. send: smart planning

`ProcessSmartOptions` (`backup/backup.go:72`) → `selectSmartSnapshots` → `planSmartSnapshots` (`backup/plan.go:94`). `plan` uses the same function, so `send` and `plan` choose the same action.

```mermaid
flowchart TD
    IN["zfs list snapshots +<br/>manifests at each destination<br/>(getBackupsForTarget)"] --> PC["PartialSetCompletable<br/>--resume, or the lagging destination<br/>has every volume of the set"]
    PC --> F{"--full?"}
    F -- yes --> FH{"full of newest *FullSuffix<br/>snapshot exists at…"}
    FH -- all --> NO1(["noop: already-backed-up"])
    FH -- "some, no --resume" --> ERR1(["error: destinations out of sync"])
    FH -- none / --resume --> FULL1(["full: explicit-full"])

    F -- no --> PS{"partial set at some<br/>destinations and completable?"}
    PS -- yes --> CP(["full or incremental:<br/>complete-partial<br/>Backup copies manifest only"])
    PS -- no --> LAST["newest backup per destination<br/>checkLastBackupsOnPool"]
    LAST -- "newer than pool" --> FUT(["ErrLastBackupInFuture: exit 2"])
    LAST --> R{"why?"}
    R -- "no full yet" --> FULL2(["full: no-previous-full"])
    R -- "last full older than<br/>--fullIfOlderThan" --> FULL3(["full: window-elapsed"])
    R -- "incremental source<br/>pruned from pool" --> FULL4(["full: source-pruned"])
    R -- "newer *IncrSuffix snapshot" --> INC(["incremental: newer-candidate"])
    R -- "nothing newer" --> NO2(["noop: nothing-newer"])
```

## 5. send: data pipeline

All stages run as goroutines in one `errgroup`. A failure in any stage cancels `ctx` and stops all stages. No final manifest is written after a failure.

```mermaid
flowchart LR
    ZFS[["zfs send"]] -- "stdout<br/>io.Pipe" --> SPL

    subgraph SPL["sendStream (backup.go:1044)"]
        direction TB
        H["SHA-256 of whole stream<br/>(for --resume)"]
        CUT["cut at volsize − 50 KiB<br/>CreateBackupVolume"]
    end

    SPL -- startCh --> FWD["forwarder<br/>pending.add()"]
    FWD -- stepCh --> D1["retryUploadChainer<br/>dest 1<br/>MaxParallelUploads workers<br/>exp. backoff"]
    D1 --> D2["retryUploadChainer<br/>dest 2"]
    D2 --> DEL["retryUploadChainer<br/>delete://<br/>(only if maxFileBuffer > 0)"]
    DEL --> FIN["finisher<br/>append to jobInfo.Volumes<br/>saveManifest(partial) → cache<br/>pending.done()"]
    FIN -. "fileBuffer token" .-> SPL

    FM["final manifest goroutine<br/>wait pending == 0<br/>saveManifest(final)"] -- "manifest volume<br/>into stepCh" --> FWD
```

Key points:

- Destinations form a **chain**, not a fan-out. Dest 2 gets a volume only after dest 1 uploads it.
- The `delete://` backend is the last link. It deletes the temp file after every real destination has the volume.
- `fileBuffer` tokens limit the number of temp files on disk (`--maxFileBuffer`).
- `--maxFileBuffer 0` uses pipes, not temp files. Then only one destination is allowed and a failed upload cannot retry.
- The manifest goes through the same chain last. A set is complete only when its manifest is at the destination.

## 6. One volume: writer layers

`files.prepareVolume` (`files/volumeinfo.go:607`) and `CreateSimpleVolume`. Read is the reverse in `VolumeInfo.Extract`.

```mermaid
flowchart LR
    IN["bytes from splitter"] --> C{"compressor"}
    C -- "gzip (internal)" --> GZ["gzip.Writer"]
    C -- "external, e.g. xz" --> EXT[["xz -c -N"]]
    C -- "none / zfs" --> PG
    GZ --> PG
    EXT --> PG
    PG{"--encryptTo /<br/>--signFrom?"} -- yes --> PGP["openpgp Encrypt+Sign<br/>or Sign"]
    PG -- no --> CNT
    PGP --> CNT["byte counter"]
    CNT --> HS["SHA-256, CRC32C,<br/>MD5, SHA-1"]
    HS --> BUF["bufio"]
    BUF --> OUT{"maxFileBuffer"}
    OUT -- "> 0" --> TMP[("temp file<br/>workdir/temp/…")]
    OUT -- "0" --> PIPE["io.Pipe → backend Upload"]
```

The manifest always uses internal gzip. Its hashes are of the **stored** bytes (after compress and pgp). `receive` checks size + SHA-256 of each download against the manifest.

## 7. receive: data pipeline

`backup.Receive` (`backup/restore.go:253`).

```mermaid
flowchart LR
    RM["readCachedManifest<br/>(sync from destination if needed)"] --> CD["CheckDecompressor<br/>only known or --compressor"]
    CD --> PD["backend.PreDownload<br/>(S3 Glacier restore)"]
    PD --> Q["downloadChannel<br/>one entry per volume"]
    Q --> W["N download workers<br/>N = maxFileBuffer<br/>backoff retry<br/>check size + SHA-256"]
    W -- "per-volume channel" --> ORD["orderer<br/>emits volumes in order"]
    ORD --> RX["receiveStream<br/>Extract: pgp verify/decrypt<br/>→ decompress"]
    RX -- stdin --> ZR[["zfs receive"]]
```

A size or SHA-256 mismatch is a permanent error. The retry loop does not try again.

## 8. receive --auto: chain walk

`backup.AutoRestore` (`backup/restore.go:54`).

```mermaid
flowchart TD
    A["syncCache + read manifests<br/>of this dataset only"] --> L["linkManifests<br/>set ParentSnap on each incremental<br/>(a full is the preferred parent)"]
    L --> T["target = given snapshot,<br/>or newest backup"]
    T --> BA["backupThatApplies<br/>prefer an incremental whose chain<br/>reaches a local snapshot, else the full"]
    BA --> LOOP{"target snapshot<br/>already local?"}
    LOOP -- yes --> RUN
    LOOP -- no --> ADD["add to restore list"]
    ADD --> K{"full backup?<br/>or source is local?"}
    K -- yes --> RUN["Receive each item,<br/>oldest first"]
    K -- no --> PAR{"ParentSnap<br/>exists?"}
    PAR -- no --> ERR(["error: parent not found"])
    PAR -- "yes (loop check)" --> NEXT["target = ParentSnap"] --> LOOP
```

## 9. clean

`backup.Clean` (`backup/clean.go:176`). It deletes only objects it can name as a backup volume of a dataset that a manifest at this destination names.

```mermaid
flowchart TD
    A["prepareBackend, getCacheDir"] --> R["readForClean<br/>list all objects, sync + read manifests"]
    R --> LK["take send lock of each dataset<br/>busy dataset → leave it alone"]
    LK --> AGAIN{"new dataset<br/>in this read?"}
    AGAIN -- "yes (max rounds)" --> R
    AGAIN -- no --> Z{"objects but<br/>no manifests?"}
    Z -- yes --> ZE(["refuse: check --manifestPrefix"])
    Z -- no --> LO["local-only cached manifests<br/>--cleanLocal → delete later<br/>else keep their volumes"]
    LO --> CAND["candidates = objects that<br/>• are not manifests<br/>• are not in a nested destination<br/>• parse as a volume name<br/>• belong to a known, non-busy dataset"]
    CAND --> KEEP{"each manifest:<br/>all volumes present?"}
    KEEP -- "yes, or no --force" --> KV["keep its volumes<br/>(warn if broken)"]
    KEEP -- "missing + --force" --> FM["delete its manifest<br/>and its volumes"]
    KV --> DEL["deletes = candidates − keep<br/>+ forced manifests"]
    FM --> DEL
    DEL --> DRY{"--dry-run?"}
    DRY -- yes --> LOG(["log what would be deleted"])
    DRY -- no --> GO["5 workers, backend.Delete<br/>with backoff"]
    GO --> LOC["then remove local cache files"]
```

## 10. Storage layout

```mermaid
flowchart LR
    subgraph host["Host: --workingDirectory (default ~/.zfsbackup)"]
        direction TB
        LOCKS["locks/<br/>md5(dataset).lck"]
        CACHE["cache/md5(canonical URI)/<br/>md5(manifest object name)"]
        TEMP["temp/zfsbackupNNN/<br/>volume temp files"]
    end

    subgraph dest["Destination URI, e.g. s3://bucket/prefix/"]
        direction TB
        MAN["manifests|tank/data|snap1.manifest.gz.pgp"]
        V1["tank/data|snap1.zstream.gz.pgp.vol1"]
        V2["tank/data|snap1.zstream.gz.pgp.vol2"]
        INC["tank/data|snap1|to|snap2.zstream.gz.pgp.vol1"]
    end

    CACHE -. "copy of" .-> MAN
    MAN -- "lists Volumes[]<br/>size + SHA-256 each" --> V1
    MAN --> V2
```

- Separator default is `|` (`--separator`). Incremental names are `<vol>|<from>|to|<to>`.
- `.gz` is the compressor; `.pgp` appears when you encrypt or sign.
- The cache key is the **canonical** URI. `getCacheDir` moves an old cache dir (keyed by the typed URI) into it.
- The lock lives in the working directory. `send` and `clean` see each other only on the same host with the same `--workingDirectory`.

## 11. plan: simulator

`cmd.runPlan` (`cmd/plan.go:143`), `Scenario.Run` (`backup/plan.go:340`), `Simulation.Check` (`backup/plan_checks.go:77`). `plan` never writes to a destination.

```mermaid
flowchart TD
    IN1["snapshots:<br/>zfs list, or --snapshots file"] --> SC["Scenario"]
    IN2["manifests:<br/>destination, --manifests file, or none"] --> SC
    IN3["--schedule spec<br/>sanoid policy, until=, skip=,<br/>location=, snapshot-delay="] --> SC
    SC --> RUN{"until= set?"}
    RUN -- no --> ONE["one step: planSmartSnapshots now"]
    RUN -- yes --> LOOP["each cron run:<br/>Schedule.Advance (take + prune snapshots)<br/>skip= → host down<br/>planSmartSnapshots<br/>add fake manifest"]
    ONE --> CHK
    LOOP --> CHK["Check: no-errors, chain-links,<br/>source-present, no-duplicate-send,<br/>no-orphan-full, full-cadence,<br/>restore-depth, coverage, only"]
    CHK --> OUT["WriteText / WriteJSON"]
    OUT --> EX{"violations?"}
    EX -- yes --> E2(["exit 2"])
    EX -- no --> E0(["exit 0"])
```
