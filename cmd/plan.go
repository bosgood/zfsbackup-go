// Copyright © 2016 Prateek Malhotra (someone1@gmail.com)
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/zfs"
)

var (
	planSnapshots string
	planManifests string
	planSchedule  string

	// errChecksFailed makes `plan` exit with status 2.
	errChecksFailed = errors.New("plan checks failed")
)

// planCmd represents the plan command
var planCmd = &cobra.Command{
	Use:   "plan [flags] volume [uri(s)]",
	Short: "plan shows what a smart backup would send, without sending anything.",
	Long: `plan runs the smart backup decision of "send" (--full, --increment or
--fullIfOlderThan, with --snapshotPrefix and the snapshot suffixes) and prints
what the next run would back up and why. With --schedule it projects the runs
over time as sanoid takes and prunes snapshots. The output ends with invariant
checks (restore chains, duplicate sends, full cadence, ...): plan exits 0 when
they pass, 2 when any fails, and 1 on other errors.

Snapshots come from the pool (zfs list, see --zfsPath) unless --snapshots
names a listing. Destination URIs are read for the backups already there,
syncing the local manifest cache; --manifests lists them in a file instead.
Without either, the destination is empty.

Text output matches the scenario goldens (backup/testdata/scenarios), so
"plan ... > expected.txt" records a new one.`,
	PreRunE: validatePlanFlags,
	RunE:    runPlan,
}

func init() {
	RootCmd.AddCommand(planCmd)

	backup.AddSmartFlags(planCmd.Flags(), &jobInfo)
	planCmd.Flags().BoolVar(
		&jobInfo.Resume,
		"resume",
		false,
		"plan as `send --resume` would: complete a backup set missing at some destinations even where its volumes are not all there.",
	)
	planCmd.Flags().StringVar(
		&planSnapshots,
		"snapshots",
		"",
		"read the snapshots from this file (- for stdin) instead of the pool: the output of "+
			"`zfs list -H -p -t snapshot,bookmark -o name,creation,type -S creation <volume>`, bare sanoid names, or a JSON "+
			"array of rows with a name and optionally a creation epoch and a type.",
	)
	planCmd.Flags().StringVar(
		&planManifests,
		"manifests",
		"",
		"read the backups already at the destinations from this file instead of destination URIs: one `<base>` (full) or "+
			"`<source> to <base>` (incremental) per line, with `---` between destinations.",
	)
	planCmd.Flags().StringVar(
		&planSchedule,
		"schedule",
		"",
		"project runs over time, e.g. \"policy=hourly=36,daily=30,monthly=3,until=2027-10-01T00:00:00Z,every=24h\". Also "+
			"from= (default: an hour after the newest snapshot), snapshot-delay= (how long after a boundary sanoid takes its "+
			"snapshots, e.g. 3m), skip=<from>..<until> (no runs in that range; repeatable), checks=coverage:_monthly, and "+
			"location= for sanoid names in a time zone other than UTC.",
	)
}

func validatePlanFlags(cmd *cobra.Command, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		_ = cmd.Usage()
		return errInvalidInput
	}
	if strings.Contains(args[0], "@") {
		log.AppLogger.Errorf("plan takes a volume, not a snapshot: %s", args[0])
		return errInvalidInput
	}
	if err := validateSmartFlags(); err != nil {
		return err
	}
	if planSnapshots == "-" && planManifests == "-" {
		log.AppLogger.Errorf("--snapshots and --manifests cannot both read stdin.")
		return errInvalidInput
	}
	if len(args) == 2 {
		if planManifests != "" {
			log.AppLogger.Errorf("--manifests and destination URIs are mutually exclusive.")
			return errInvalidInput
		}
		destinations, _ := parseDestinations(args[1])
		for _, destination := range destinations {
			if _, err := backends.GetBackendForURI(destination); err != nil {
				log.AppLogger.Errorf("Unsupported destination URI %s - %v", backends.RedactURI(destination), err)
				return err
			}
		}
		// Manifests may be encrypted and/or signed: load the keys a smart
		// send loads to read them, so the same flags work for both.
		return loadSendKeys()
	}
	return nil
}

func runPlan(cmd *cobra.Command, args []string) error {
	sc := &backup.Scenario{JobInfo: jobInfo}
	if planSchedule != "" {
		if err := sc.ParseScheduleSpec(planSchedule); err != nil {
			log.AppLogger.Errorf("Invalid --schedule - %v", err)
			return err
		}
	}
	sc.Volume = args[0]

	if planSnapshots == "" {
		snapshots, err := zfs.GetSnapshotsAndBookmarks(cmd.Context(), sc.Volume)
		if err != nil {
			log.AppLogger.Errorf("Could not list the snapshots of %s - %v", sc.Volume, err)
			return err
		}
		sc.Snapshots = snapshots
	} else if err := readPlanInput(planSnapshots, sc.ReadSnapshots); err != nil {
		return err
	} else if sc.CaptureDataset != "" && sc.CaptureDataset != sc.Volume {
		log.AppLogger.Errorf("--snapshots lists %s, not %s.", sc.CaptureDataset, sc.Volume)
		return errInvalidInput
	}

	switch {
	case planManifests != "":
		if err := readPlanInput(planManifests, sc.ReadManifests); err != nil {
			return err
		}
	case len(args) == 2:
		jobInfo.Destinations, jobInfo.DestinationsAsTyped = parseDestinations(args[1])
		for _, destination := range jobInfo.Destinations {
			manifests, err := backup.BackupsAtTarget(cmd.Context(), sc.Volume, destination, &jobInfo)
			if err != nil {
				log.AppLogger.Errorf("Could not read the backups at %s - %v", backends.RedactURI(destination), err)
				return err
			}
			sc.DestBackups = append(sc.DestBackups, manifests)
		}
		// A capture without creation times dates snapshots from their names,
		// seconds before the time the manifests hold; send would then see the
		// backed-up snapshot as pruned. Take the manifests' word for it.
		if adopted := sc.AdoptCreationTimes(); len(sc.NameDated) > 0 {
			log.AppLogger.Warningf("The capture has no creation times: %d taken from the manifests at the destination, "+
				"the rest from the snapshot names. Capture `zfs list -H -p -o name,creation -t snapshot,bookmark -S creation %s` "+
				"to plan exactly what send will do.", adopted, sc.Volume)
		}
		// A set whose manifest is missing at some destinations is completed there, as
		// send would, when its volumes are there.
		completable, err := backup.PartialSetCompletable(cmd.Context(), &jobInfo, sc.DestBackups)
		if err != nil {
			log.AppLogger.Errorf("Could not check the destinations for a partial backup set - %v", err)
			return err
		}
		sc.Completable = completable
	default:
		sc.DestBackups = [][]*files.JobInfo{{}}
	}

	sim := sc.Run()
	if len(sim.Steps) == 0 {
		log.AppLogger.Errorf("Nothing to plan: --schedule until=%v is before the first run.", sc.Until)
		return errInvalidInput
	}
	violations := sim.Check(sc.Checks)
	write := sim.WriteText
	if config.JSONOutput {
		write = sim.WriteJSON
	}
	if err := write(config.Stdout, violations); err != nil {
		return err
	}
	if len(violations) > 0 {
		return errChecksFailed
	}
	return nil
}

// readPlanInput reads a --snapshots or --manifests file, or stdin for "-".
func readPlanInput(path string, read func(io.Reader) error) error {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			log.AppLogger.Errorf("Could not open %s - %v", path, err)
			return err
		}
		defer f.Close()
		r = f
	}
	if err := read(r); err != nil {
		log.AppLogger.Errorf("Could not read %s - %v", path, err)
		return err
	}
	return nil
}
