// Copyright © 2017 Prateek Malhotra (someone1@gmail.com)
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
	"strings"

	"github.com/spf13/cobra"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/log"
)

var cleanLocal bool
var cleanDryRun bool

// cleanCmd represents the clean command
var cleanCmd = &cobra.Command{
	Use:   "clean [flags] uri",
	Short: "Clean deletes backup volumes at the destination that no manifest there lists.",
	Long: `Clean deletes backup volumes at the destination that no manifest lists, for the datasets
that have manifests there. It never deletes manifests or objects it cannot parse as a
backup volume, refuses a destination with objects but no manifests under
--manifestPrefix, and leaves alone any dataset a send on this host is working on.

--force also deletes broken backup sets (manifest included) at the destination whose
volumes are missing. It leaves cached manifests that are not at the destination alone.
--cleanLocal also deletes cached manifests that are not at the destination, and their
volumes at the destination; those manifests are what --resume continues from. Cached
manifests of another --manifestPrefix are never deleted.`,
	SilenceErrors: true,
	PreRunE:       validateCleanFlags,
	RunE: func(cmd *cobra.Command, args []string) error {
		jobInfo.Destinations, jobInfo.DestinationsAsTyped = parseDestinations(args[0])
		return backup.Clean(cmd.Context(), &jobInfo, cleanLocal, cleanDryRun)
	},
}

func init() {
	RootCmd.AddCommand(cleanCmd)

	cleanCmd.Flags().BoolVarP(&cleanLocal, "cleanLocal", "", false, "Delete cached manifests that are not at the destination, and delete their volumes at the destination.")
	cleanCmd.Flags().BoolVarP(&cleanDryRun, "dry-run", "n", false, "Do not delete anything; only log what would be deleted.")
	cleanCmd.Flags().BoolVarP(&jobInfo.Force, "force", "", false,
		"Also delete broken backup sets at the destination (sets where volumes expected in the manifest file are not found), manifest included. "+
			"Cached manifests that are not at the destination are left alone; see --cleanLocal. Use with caution.",
	)
}

func validateCleanFlags(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		_ = cmd.Usage()
		return errInvalidInput
	}
	if strings.Contains(args[0], ",") {
		log.AppLogger.Errorf("clean takes one destination, got %s.", backends.RedactURI(args[0]))
		return errInvalidInput
	}

	if err := loadReceiveKeys(); err != nil {
		return err
	}

	return nil
}
