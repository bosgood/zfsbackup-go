{
  lib,
  config,
  pkgs,
  ...
}:

let
  cfg = config.mine.zfsbackup;
  inherit (lib)
    mkIf
    mkEnableOption
    mkOption
    types
    ;
  inherit (builtins)
    filter
    map
    listToAttrs
    replaceStrings
    ;

  # Turn "ypool/user/app/readeck" into a systemd-safe unit name suffix.
  unitNameOf = dataset: replaceStrings [ "/" ] [ "-" ] dataset;

  # Build zfsbackup-go from our fork. The source is pinned by `source.rev`
  # alone: builtins.fetchGit derives the content hash from the commit, so no
  # separate source hash is needed. To update, just bump `mine.zfsbackup.source.rev`.
  zfsbackup = pkgs.buildGoModule {
    pname = "zfsbackup-go";
    version = "unstable-${cfg.source.rev}";

    # A commit SHA fully determines the tree, so fetchGit needs no declared
    # hash. The default "master" tracks the branch tip; set source.rev to a full
    # SHA for reproducible builds. fetchGit's `rev` takes a 40-char SHA while
    # `ref` takes a branch/tag name, so dispatch on the value's shape. allRefs
    # lets fetchGit reach a commit that isn't at the default branch tip.
    src =
      let
        isSha = builtins.match "[0-9a-f]{40}" cfg.source.rev != null;
      in
      builtins.fetchGit (
        {
          url = "https://github.com/${cfg.source.owner}/${cfg.source.repo}.git";
          allRefs = true;
        }
        // (if isSha then { rev = cfg.source.rev; } else { ref = cfg.source.rev; })
      );

    # Matches the fork's Makefile `build` target (gox -ldflags="-w -s"): strip
    # symbol table and DWARF debug info for a smaller binary. The Makefile only
    # uses gox to cross-compile; buildGoModule already targets the host, so no
    # other Makefile logic (no version stamping, tags, or codegen) applies.
    ldflags = [
      "-s"
      "-w"
    ];

    # null requires the fork to vendor its deps (run `go mod vendor` and commit
    # the vendor/ dir); then bumping source.rev is all that's ever needed. If
    # the fork is NOT vendored, set source.vendorHash to the sha256 the build
    # prints on its first failure.
    vendorHash = cfg.source.vendorHash;

    # Upstream's main package lives at the repo root and produces a
    # `zfsbackup-go` binary; no `subPackages` override needed.

    meta = with lib; {
      description = "ZFS snapshot backup to cloud object storage (bosgood fork)";
      homepage = "https://github.com/${cfg.source.owner}/${cfg.source.repo}";
      license = licenses.mit;
      mainProgram = "zfsbackup-go";
    };
  };

  # Where the main script stashes the byte count for statusScript to read.
  # Leading dot keeps node_exporter (which only scrapes *.prom) from touching it.
  bytesFileOf = d: "${d.metrics.textfileDir}/.zfsbackup-${unitNameOf d.dataset}.bytes";

  # Publish per-dataset backup status as node_exporter textfile metrics when the
  # grafana/monitoring module is enabled. Runs as ExecStopPost so it records the
  # outcome of every run (success or failure).
  statusScript = d: pkgs.writeShellScript "zfsbackup-status-${unitNameOf d.dataset}" ''
    set -eu
    PATH=${lib.makeBinPath [ pkgs.coreutils ]}
    if [ "''${SERVICE_RESULT:-success}" = "success" ] && [ "''${EXIT_STATUS:-0}" = "0" ]; then
      ok=1
    else
      ok=0
    fi
    # Byte count recorded by the main script; missing/garbled => 0.
    bytes=0
    if [ -r "${bytesFileOf d}" ]; then
      bytes="$(cat "${bytesFileOf d}")"
    fi
    case "$bytes" in
      ""|*[!0-9]*) bytes=0 ;;
    esac
    out="${d.metrics.textfileDir}/zfsbackup-${unitNameOf d.dataset}.prom"
    tmp="$(mktemp "${d.metrics.textfileDir}/zfsbackup-${unitNameOf d.dataset}.prom.XXXXXX")"
    {
      echo '# HELP zfsbackup_last_run_timestamp_seconds Unix time of the last zfsbackup run.'
      echo '# TYPE zfsbackup_last_run_timestamp_seconds gauge'
      printf 'zfsbackup_last_run_timestamp_seconds{dataset="%s"} %s\n' "${d.dataset}" "$(date +%s)"
      echo '# HELP zfsbackup_last_success 1 if the last zfsbackup run succeeded, 0 otherwise.'
      echo '# TYPE zfsbackup_last_success gauge'
      printf 'zfsbackup_last_success{dataset="%s"} %d\n' "${d.dataset}" "$ok"
      echo '# HELP zfsbackup_last_run_bytes_uploaded Bytes uploaded by the last zfsbackup run (0 when nothing new to sync).'
      echo '# TYPE zfsbackup_last_run_bytes_uploaded gauge'
      printf 'zfsbackup_last_run_bytes_uploaded{dataset="%s"} %s\n' "${d.dataset}" "$bytes"
    } > "$tmp"
    chmod 0644 "$tmp"
    mv -f "$tmp" "$out"
  '';

  # A location's inventory is keyed by its (s3Destination, awsRegion) pair.
  # Sanitize both into one unit- and filename-safe slug; "s3://b/zfsbackup/" +
  # "us-east-2" -> "b-zfsbackup-us-east-2".
  locNameOf =
    loc:
    lib.removeSuffix "-" (
      replaceStrings [ "s3://" "/" ":" "." ] [ "" "-" "-" "-" ] loc.s3Destination
    )
    + "-"
    + loc.awsRegion;

  listPromOf = loc: "${cfg.list.textfileDir}/zfsbackup-list-${locNameOf loc}.prom";

  # Refresh one location's backup inventory: run `zfsbackup-go list`, then hand
  # the JSON (and the list exit code) to list-metrics.py, which writes the .prom
  # file atomically. The unit is not failed on a list error; the script records
  # zfsbackup_list_scrape_success=0 so a broken lister surfaces in Prometheus
  # rather than going silent. The listing only contains manifests for completed
  # uploads, so these metrics describe backups at rest, never one in flight.
  listScript =
    loc:
    pkgs.writeShellScript "zfsbackup-list-${locNameOf loc}" ''
      set -u
      # node_exporter owns the textfile dir; fail loudly rather than create it,
      # so a missing collector dir surfaces as a unit failure (not silent files).
      if [ ! -d "${cfg.list.textfileDir}" ]; then
        echo "zfsbackup-list: textfile dir ${cfg.list.textfileDir} does not exist" >&2
        exit 1
      fi
      out="$(${pkgs.coreutils}/bin/mktemp)"
      trap '${pkgs.coreutils}/bin/rm -f "$out"' EXIT
      start="$(${pkgs.coreutils}/bin/date +%s)"
      # stdout carries the JSON document; stderr flows to the journal. Flags go
      # before the positional destination (as in the send path). For a GPG
      # destination, --encryptTo + --secretKeyRingPath let list decrypt manifests.
      zfsbackup-go list \
        --jsonOutput \
        ${lib.optionalString (loc.encryptTo != null) "--encryptTo \"${loc.encryptTo}\""} \
        ${lib.optionalString (loc.secretKeyFile != null) "--secretKeyRingPath \"${loc.secretKeyFile}\""} \
        "${loc.s3Destination}" > "$out"
      rc=$?
      end="$(${pkgs.coreutils}/bin/date +%s)"
      ${pkgs.python3}/bin/python3 ${./list-metrics.py} \
        --s3 "${loc.s3Destination}" \
        --region "${loc.awsRegion}" \
        --json "$out" \
        --rc "$rc" \
        --out "${listPromOf loc}" \
        --scrape-time "$end" \
        --duration "$((end - start))" \
        ${lib.optionalString (!cfg.list.includeManifests) "--no-manifests"}
    '';

  # Known good zfsbackup-go command this module reproduces:
  #   AWS_PROFILE=default AWS_REGION=us-east-2 zfsbackup-go send \
  #     --logLevel info --fullIfOlderThan 24h \
  #     ypool/user/app/readeck s3://example-upload-bucket-name/zfsbackup/
in
{
  options.mine.zfsbackup = {
    enable = mkEnableOption "zfsbackup-go scheduled backups to S3";

    source = {
      owner = mkOption {
        type = types.str;
        description = "GitHub owner of the zfsbackup-go fork to build.";
        default = "bosgood";
      };

      repo = mkOption {
        type = types.str;
        description = "GitHub repo name of the zfsbackup-go fork to build.";
        default = "zfsbackup-go";
      };

      rev = mkOption {
        type = types.str;
        description = ''
          A full 40-char commit SHA, or a branch/tag name, in the fork to build.
          Pin to a SHA for reproducible builds. A branch name (the default
          "master") tracks the branch tip and re-fetches on each evaluation,
          which is not reproducible.
        '';
        example = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2";
        default = "master";
      };

      vendorHash = mkOption {
        type = types.nullOr types.str;
        description = ''
          Hash of the vendored Go dependencies. null (the default) requires the
          fork to vendor its deps in-tree (commit a vendor/ dir); then only rev
          ever needs to change. For a non-vendored fork, set this to the sha256
          the build prints on its first failure. Changes whenever go.mod /
          go.sum change in the pinned revision.
        '';
        default = null;
      };
    };

    datasets = mkOption {
      description = "ZFS datasets to back up, with their per-dataset schedule.";
      type = types.listOf (types.submodule {
        options = {
          awsProfile = mkOption {
            description = "AWS_PROFILE used to authenticate to S3. Credentials must exist at /root/.aws/credentials.";
            type = types.str;
            default = "default";
          };

          awsRegion = mkOption {
            description = "AWS region containing the destination bucket.";
            type = types.str;
            example = "us-east-2";
          };

          publicKeyFile = mkOption {
            description = ''
              ASCII-armored GPG public keyring used as the encryption recipient,
              passed to zfsbackup-go as --publicKeyRingPath. Accepts either form:
              a path literal (e.g. ./zfsbackups.pub.asc), which Nix copies into
              the store; or an absolute string path (e.g. "/etc/zfsbackup/key.asc"),
              which is used verbatim. A public key is safe to keep in the store.
            '';
            type = lib.types.nullOr lib.types.path;
            example = lib.literalExpression "./zfsbackups.pub.asc";
            default = null;
          };

          secretKeyFile = mkOption {
            description = ''
              ASCII-armored GPG secret keyring used for signing, passed to
              zfsbackup-go as --secretKeyRingPath. Accepts either form: a path
              literal, which Nix copies into the store; or an absolute string
              path, which is used verbatim. WARNING: the Nix store is
              world-readable, so a private key should be referenced as an
              out-of-band string path (e.g. "/usr/keys/zfs/backups.sec.asc"),
              not a path literal.
            '';
            type = lib.types.nullOr lib.types.path;
            example = "/usr/keys/zfs/backups.sec.asc";
            default = null;
          };

          encryptTo = mkOption {
            description = "GPG identifier (email or key ID) to encrypt backups to. Must match a key in publicKeyFile.";
            type = lib.types.nullOr types.str;
            example = "name@example.com";
            default = null;
          };

          signFrom = mkOption {
            description = "Email of the user to sign on behalf of (from the provided private keyring)";
            type = lib.types.nullOr types.str;
            example = "name@example.com";
            default = null;
          };

          raw = mkOption {
            description = "Use zfs-send --raw option. This allows sending of encrypted datasets in their cyphertext form, preserving the encryption";
            type = types.bool;
            default = false;
          };

          s3Destination = mkOption {
            description = "S3 destination URL passed to zfsbackup-go send.";
            type = types.str;
            example = "s3://bucket-name/dirname/";
            default = null;
          };

          fullIfOlderThan = mkOption {
            description = "Take a new full backup if the last full is older than this duration.";
            type = types.str;
            default = "720h"; # 30d; the manual example used 24h.
          };

          dataset = mkOption {
            type = types.str;
            example = "ypool/user/app/readeck";
          };

          schedule = mkOption {
            type = types.nullOr types.str;
            description = ''
              systemd OnCalendar expression (e.g. "daily", "*-*-* 03:00:00").

              null generates the service but no timer, so the backup never fires
              on its own and only runs when started by hand:

                systemctl start zfsbackup-ypool-enc-user-app-stash
                journalctl -fu zfsbackup-ypool-enc-user-app-stash

              Everything else about the run (flags, encryption, bookmarks,
              metrics) is identical to a scheduled dataset's.
            '';
            default = "daily";
          };

          bookmarks = {
            enable = mkEnableOption ''
              creating a ZFS bookmark for every backed-up snapshot, so an
              incremental send survives sanoid pruning its source snapshot.

              zfsbackup-go picks its incremental base from the snapshot recorded
              in the destination's last manifest. sanoid autoprunes on its own
              retention schedule, so once a backup falls behind that window the
              base snapshot is gone locally and the send fails outright
              ("selected incremental snapshot does not exist"). A bookmark keeps
              the base's birth txg alive without keeping its data, which is all
              `zfs send -i` needs. zfsbackup-go already resolves a missing base
              to a same-named bookmark and switches to the `ds#name` send form,
              so creating the bookmarks is the only missing piece'';

            pattern = mkOption {
              type = types.str;
              description = ''
                Extended regex selecting which snapshots to bookmark, matched
                against the full `dataset@name`. The same expression selects
                which bookmarks `keep` prunes, so it must match the bookmark
                form (`dataset#name`) too -- anchor on the suffix, not the "@".
                The default matches sanoid's autosnap naming.
              '';
              default = "_(hourly|daily|weekly|monthly|yearly)$";
            };

            keep = mkOption {
              type = types.int;
              description = ''
                How many of the most recent matching bookmarks to retain; older
                ones are destroyed after each send. Only the single bookmark
                named by the destination's newest manifest is strictly needed,
                but a bookmark costs no space, so the default keeps ample slack
                for a destination that has fallen several runs behind.
              '';
              default = 30;
            };
          };

          metrics = {
            enable = mkEnableOption "Publish backup status as node_exporter textfile metrics";

            textfileDir = mkOption {
              type = types.str;
              description = "Directory scraped by node_exporter's textfile collector for backup status metrics";
              default = "/var/lib/node_exporter/textfile_collector";
            };
          };
        };
      });

      default = [ ];
    };

    list = {
      enable = mkEnableOption ''
        publishing a per-location inventory of existing backups, scraped with
        `zfsbackup-go list`, as node_exporter textfile metrics. One .prom file is
        written per location, refreshed on a timer'';

      textfileDir = mkOption {
        type = types.str;
        description = "Directory scraped by node_exporter's textfile collector for the inventory metrics.";
        default = "/var/lib/node_exporter/textfile_collector";
      };

      schedule = mkOption {
        type = types.str;
        description = ''
          systemd OnCalendar expression for refreshing the inventory. Listing
          only reflects completed uploads, so there is little value in scraping
          much more often than backups run; "daily" tracks a daily send cadence.
        '';
        default = "daily";
      };

      includeManifests = mkOption {
        type = types.bool;
        description = ''
          Also emit per-manifest metrics (one labelled set per retained backup,
          with full metadata) on top of the default scrape- and per-volume-level
          summary. This is the richest view, but its series count grows with the
          number of retained incrementals (≈9 series per manifest per location),
          so it is off by default; enable it when you want every backup's
          metadata in Prometheus.
        '';
        default = false;
      };

      locations = mkOption {
        description = "S3 locations to inventory. Each produces one .prom file named after the location.";
        type = types.listOf (types.submodule {
          options = {
            s3Destination = mkOption {
              type = types.str;
              example = "s3://example-upload-bucket-name/zfsbackup/";
              description = "S3 URL passed to `zfsbackup-go list`.";
            };

            awsRegion = mkOption {
              type = types.str;
              example = "us-east-2";
              description = "AWS region containing the bucket.";
            };

            awsProfile = mkOption {
              type = types.str;
              description = "AWS_PROFILE used to authenticate to S3. Credentials must exist at /root/.aws/credentials.";
              default = "default";
            };

            encryptTo = mkOption {
              description = ''
                GPG identifier (email or key ID) the manifests at this location
                are encrypted to, passed to `zfsbackup-go list` as --encryptTo.
                Required for a destination of GPG-encrypted backups: list decodes
                every manifest, and encrypted manifests only parse when the
                decryption identity is supplied. Leave null for plaintext/raw
                destinations (setting it there makes list fail). Mirrors the
                per-dataset encryptTo.
              '';
              type = lib.types.nullOr types.str;
              example = "name@example.com";
              default = null;
            };

            secretKeyFile = mkOption {
              description = ''
                ASCII-armored GPG secret keyring used to decrypt the manifests,
                passed to `zfsbackup-go list` as --secretKeyRingPath. Accepts a
                path literal (copied into the store) or an absolute string path
                (used verbatim); prefer an out-of-band string path so a private
                key never lands in the world-readable Nix store. Mirrors the
                per-dataset secretKeyFile and is required when encryptTo is set.
              '';
              type = lib.types.nullOr lib.types.path;
              example = "/usr/keys/zfs/backups.sec.asc";
              default = null;
            };
          };
        });
        default = [ ];
      };
    };
  };

  config = mkIf cfg.enable {
    environment.systemPackages = [ zfsbackup ];

    # zfsbackup-go's --encryptTo needs a public keyring containing the
    # recipient, and --signFrom needs a secret keyring containing the signer.
    # Fail the build early rather than at timer-fire time if either is missing.
    assertions = lib.flatten (map (d: [
      {
        assertion = d.encryptTo == null || d.publicKeyFile != null;
        message = "mine.zfsbackup: dataset ${d.dataset} sets encryptTo but no publicKeyFile; --encryptTo requires a public keyring.";
      }
      {
        assertion = d.signFrom == null || d.secretKeyFile != null;
        message = "mine.zfsbackup: dataset ${d.dataset} sets signFrom but no secretKeyFile; --signFrom requires a secret keyring.";
      }
    ]) cfg.datasets)
    # Listing a GPG destination decrypts every manifest, so encryptTo without a
    # secret keyring can only fail at timer-fire time; catch it at build instead.
    ++ map (loc: {
      assertion = loc.encryptTo == null || loc.secretKeyFile != null;
      message = "mine.zfsbackup: list location ${loc.s3Destination} sets encryptTo but no secretKeyFile; --encryptTo requires a secret keyring to decrypt manifests.";
    }) cfg.list.locations
    # Unit names are derived from the dataset alone, so listing a dataset twice
    # (say, once scheduled and once manual) silently drops one of the two
    # definitions instead of failing.
    ++ [
      {
        assertion =
          let
            names = map (d: d.dataset) cfg.datasets;
          in
          lib.length (lib.unique names) == lib.length names;
        message = "mine.zfsbackup: mine.zfsbackup.datasets lists the same dataset more than once; each dataset maps to a single systemd unit name, so the duplicates would collide.";
      }
    ];

    # One systemd service per dataset, plus a timer for the scheduled ones
    # (cron-equivalent); a schedule = null dataset gets the service only.
    systemd.services = listToAttrs (map (d: {
      name = "zfsbackup-${unitNameOf d.dataset}";
      value = {
        description = "zfsbackup-go send ${d.dataset} -> ${d.s3Destination}";
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];
        path = [ zfsbackup ];
        environment = {
          AWS_PROFILE = d.awsProfile;
          AWS_REGION = d.awsRegion;
        };
        serviceConfig = {
          Type = "oneshot";
          User = "root";
        } // lib.optionalAttrs d.metrics.enable {
          # Record run outcome to node_exporter's textfile collector. "+" runs
          # the command as root regardless of the unit's User, and ExecStopPost
          # runs even when the main script fails.
          ExecStopPost = "+${statusScript d}";
        };
        script = ''
          ${lib.optionalString d.bookmarks.enable ''
            # Bookmark every matching snapshot. `zfs bookmark` fails when the
            # bookmark already exists, which is the steady state for all but the
            # newest snapshot, so errors are discarded rather than tested for.
            # -d 1 mirrors how zfsbackup-go itself enumerates snapshots.
            bookmark_all() {
              ${pkgs.zfs}/bin/zfs list -H -o name -t snapshot -d 1 -S creation "${d.dataset}" \
                | ${pkgs.gnugrep}/bin/grep -E '${d.bookmarks.pattern}' \
                | while read -r snap; do
                    ${pkgs.zfs}/bin/zfs bookmark "$snap" "''${snap%@*}#''${snap#*@}" 2>/dev/null || true
                  done
            }
            # Runs before the send so whichever snapshot it settles on is already
            # bookmarked, and again after so a snapshot created during a long
            # send (and pruned before the next run) is covered too.
            bookmark_all
          ''}
          # zfsbackup-go's smart (incremental) send exits non-zero with
          # "nothing new to sync" when there are no new snapshots to upload.
          # That is a normal no-op, not a failure, so map it to success.
          out="$(${pkgs.coreutils}/bin/mktemp)"
          trap '${pkgs.coreutils}/bin/rm -f "$out"' EXIT
          set +e
          zfsbackup-go send \
            --logLevel info \
            --fullIfOlderThan ${d.fullIfOlderThan} \
            --zfsPath ${pkgs.zfs}/bin/zfs \
            ${lib.optionalString (d.encryptTo != null) "--encryptTo \"${d.encryptTo}\""} \
            ${lib.optionalString (d.publicKeyFile != null) "--publicKeyRingPath \"${d.publicKeyFile}\""} \
            ${lib.optionalString (d.secretKeyFile != null) "--secretKeyRingPath \"${d.secretKeyFile}\""} \
            ${lib.optionalString (d.signFrom != null) "--signFrom \"${d.signFrom}\""} \
            ${if d.raw then "--raw" else ""} \
            "${d.dataset}" \
            "${d.s3Destination}" 2>&1 | ${pkgs.coreutils}/bin/tee "$out"
          rc=''${PIPESTATUS[0]}
          set -e
          ${lib.optionalString d.bookmarks.enable ''
            bookmark_all
            # Prune to the newest `keep` bookmarks. Deliberately unconditional on
            # $rc: a failed send leaves the destination's incremental base where
            # it was, so the retention window is unchanged either way.
            ${pkgs.zfs}/bin/zfs list -H -o name -t bookmark -d 1 -S creation "${d.dataset}" \
              | ${pkgs.gnugrep}/bin/grep -E '${d.bookmarks.pattern}' \
              | ${pkgs.coreutils}/bin/tail -n +${toString (d.bookmarks.keep + 1)} \
              | ${pkgs.findutils}/bin/xargs -r -n1 ${pkgs.zfs}/bin/zfs destroy || true
          ''}
          ${lib.optionalString d.metrics.enable ''
            # Record bytes uploaded for statusScript. zfsbackup-go prints e.g.
            # "Total Bytes Written: 763 (763 B)"; absent (e.g. nothing new to
            # sync) means 0 bytes uploaded.
            bytes="$(${pkgs.gnugrep}/bin/grep "Total Bytes Written:" "$out" \
              | ${pkgs.coreutils}/bin/tail -n1 \
              | ${pkgs.gnused}/bin/sed -E 's/.*Total Bytes Written:[[:space:]]*([0-9]+).*/\1/' || true)"
            case "$bytes" in ""|*[!0-9]*) bytes=0 ;; esac
            printf '%s' "$bytes" > "${bytesFileOf d}"
          ''}
          if [ "$rc" -ne 0 ] && ${pkgs.gnugrep}/bin/grep -q "nothing new to sync" "$out"; then
            echo "zfsbackup: nothing new to sync for ${d.dataset}; treating as success"
            exit 0
          fi
          exit "$rc"
        '';
      };
    }) cfg.datasets)
    # One inventory service per S3 location (only when list metrics are enabled).
    // lib.optionalAttrs cfg.list.enable (listToAttrs (map (loc: {
      name = "zfsbackup-list-${locNameOf loc}";
      value = {
        description = "zfsbackup-go list ${loc.s3Destination} -> textfile metrics";
        after = [ "network-online.target" ];
        wants = [ "network-online.target" ];
        path = [ zfsbackup ];
        environment = {
          AWS_PROFILE = loc.awsProfile;
          AWS_REGION = loc.awsRegion;
        };
        serviceConfig = {
          Type = "oneshot";
          User = "root";
          ExecStart = "${listScript loc}";
        };
      };
    }) cfg.list.locations));

    # Datasets with schedule = null are deliberately timer-less: the service
    # exists so it can be started by hand, but nothing ever triggers it.
    systemd.timers = listToAttrs (map (d: {
      name = "zfsbackup-${unitNameOf d.dataset}";
      value = {
        description = "Schedule zfsbackup-go for ${d.dataset}";
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnCalendar = d.schedule;
          Persistent = true;
          RandomizedDelaySec = "10m";
        };
      };
    }) (filter (d: d.schedule != null) cfg.datasets))
    // lib.optionalAttrs cfg.list.enable (listToAttrs (map (loc: {
      name = "zfsbackup-list-${locNameOf loc}";
      value = {
        description = "Schedule zfsbackup-go list inventory for ${loc.s3Destination}";
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnCalendar = cfg.list.schedule;
          Persistent = true;
          RandomizedDelaySec = "5m";
        };
      };
    }) cfg.list.locations));
  };
}
