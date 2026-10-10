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
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"

	humanize "github.com/dustin/go-humanize"
	"github.com/juju/ratelimit"
	"github.com/op/go-logging"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/openpgp"
	"golang.org/x/term"

	"github.com/someone1/zfsbackup-go/backends"
	"github.com/someone1/zfsbackup-go/backup"
	"github.com/someone1/zfsbackup-go/config"
	"github.com/someone1/zfsbackup-go/files"
	"github.com/someone1/zfsbackup-go/log"
	"github.com/someone1/zfsbackup-go/pgp"
	"github.com/someone1/zfsbackup-go/zfs"
)

var (
	numCores          int
	logLevel          string
	secretKeyRingPath string
	publicKeyRingPath string
	workingDirectory  string
	trustSigners      []string
	errInvalidInput   = errors.New("invalid input")
)

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:   "zfsbackup",
	Short: "zfsbackup is a tool used to do off-site backups of ZFS volumes.",
	Long: `zfsbackup is a tool used to do off-site backups of ZFS volumes.
It leverages the built-in snapshot capabilities of ZFS in order to export ZFS
volumes for long-term storage.

zfsbackup uses the "zfs send" command to export, and optionally compress, sign,
encrypt, and split the send stream to files that are then transferred to a
destination of your choosing.`,
	PersistentPreRunE: processFlags,
	PersistentPostRun: postRunCleanup,
	SilenceErrors:     true,
	SilenceUsage:      true,
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute(ctx context.Context) {
	cmd, err := RootCmd.ExecuteContextC(ctx)
	if err != nil {
		// PersistentPostRun only runs after a successful RunE, but a smart
		// send with nothing new returns ErrNoOp from PreRunE, every day.
		postRunCleanup(cmd, nil)
	}
	if code := exitCode(cmd, err); code != 0 {
		os.Exit(code)
	}
}

// exitCode maps the outcome of a command to the process exit status: 0 on
// success and when a smart send finds nothing new to back up, 2 when plan's
// checks fail or a smart send finds the last backup newer than the pool, 1 for
// other plan errors and 255 for any other error.
func exitCode(cmd *cobra.Command, err error) int {
	switch {
	case err == nil, errors.Is(err, backup.ErrNoOp):
		return 0
	case errors.Is(err, errChecksFailed), errors.Is(err, backup.ErrLastBackupInFuture):
		return 2
	case cmd == planCmd:
		return 1
	default:
		return 255
	}
}

func init() {
	RootCmd.PersistentFlags().IntVar(
		&numCores,
		"numCores",
		2,
		"number of CPU cores to utilize. Do not exceed the number of CPU cores on the system.",
	)
	RootCmd.PersistentFlags().StringVar(
		&logLevel,
		"logLevel",
		"notice",
		"this controls the verbosity level of logging. Possible values are critical, error, warning, notice, info, debug.",
	)
	RootCmd.PersistentFlags().StringVar(
		&secretKeyRingPath,
		"secretKeyRingPath",
		"",
		"the path to the PGP secret key ring",
	)
	RootCmd.PersistentFlags().StringVar(
		&publicKeyRingPath,
		"publicKeyRingPath",
		"",
		"the path to the PGP public key ring",
	)
	RootCmd.PersistentFlags().StringVar(
		&workingDirectory,
		"workingDirectory",
		"~/.zfsbackup",
		"the working directory path for zfsbackup.",
	)
	RootCmd.PersistentFlags().StringVar(
		&jobInfo.ManifestPrefix,
		"manifestPrefix",
		"manifests", "the prefix to use for all manifest files.",
	)
	RootCmd.PersistentFlags().StringVar(
		&jobInfo.EncryptTo,
		"encryptTo",
		"",
		"the email of the user to encrypt the data to from the provided public keyring.",
	)
	RootCmd.PersistentFlags().StringVar(
		&jobInfo.SignFrom,
		"signFrom",
		"",
		"the email of the user to sign on behalf of from the provided private keyring.",
	)
	RootCmd.PersistentFlags().StringArrayVar(
		&trustSigners,
		"trustSigner",
		nil,
		"also accept manifests and volumes signed by this key (an email or fingerprint in the public keyring), e.g. "+
			"the key --signFrom named before a rotation. Repeatable; needs --signFrom.",
	)
	RootCmd.PersistentFlags().StringVar(
		&zfs.ZFSPath,
		"zfsPath",
		"zfs",
		"the path to the zfs executable.",
	)
	RootCmd.PersistentFlags().BoolVar(
		&config.JSONOutput,
		"jsonOutput",
		false,
		"dump results as a JSON string - on success only",
	)
	passphrase = []byte(os.Getenv("PGP_PASSPHRASE"))
}

func resetRootFlags() {
	jobInfo = files.JobInfo{}
	numCores = 2
	logLevel = "notice"
	secretKeyRingPath = ""
	publicKeyRingPath = ""
	workingDirectory = "~/.zfsbackup"
	jobInfo.ManifestPrefix = "manifests"
	jobInfo.EncryptTo = ""
	jobInfo.SignFrom = ""
	trustSigners = nil
	zfs.ZFSPath = "zfs"
	config.JSONOutput = false
}

// nolint:gocyclo,funlen // Will do later
func processFlags(cmd *cobra.Command, args []string) error {
	switch strings.ToLower(logLevel) {
	case "critical":
		logging.SetLevel(logging.CRITICAL, log.LogModuleName)
	case "error":
		logging.SetLevel(logging.ERROR, log.LogModuleName)
	case "warning", "warn":
		logging.SetLevel(logging.WARNING, log.LogModuleName)
	case "notice":
		logging.SetLevel(logging.NOTICE, log.LogModuleName)
	case "info":
		logging.SetLevel(logging.INFO, log.LogModuleName)
	case "debug":
		logging.SetLevel(logging.DEBUG, log.LogModuleName)
	default:
		log.AppLogger.Errorf("Invalid log level provided. Was given %s", logLevel)
		return errInvalidInput
	}

	if numCores <= 0 {
		log.AppLogger.Errorf("The number of cores to use provided is an invalid value. It must be greater than 0. %d was given.", numCores)
		return errInvalidInput
	}

	if numCores > runtime.NumCPU() {
		log.AppLogger.Warningf(
			"Ignoring user provided number of cores (%d) and using the number of detected cores (%d).",
			numCores, runtime.NumCPU(),
		)
		numCores = runtime.NumCPU()
	}
	log.AppLogger.Infof("Setting number of cores to: %d", numCores)
	runtime.GOMAXPROCS(numCores)

	if secretKeyRingPath != "" {
		if err := pgp.LoadPrivateRing(secretKeyRingPath); err != nil {
			log.AppLogger.Errorf("Could not load private keyring due to an error - %v", err)
			return errInvalidInput
		}
	}
	log.AppLogger.Infof("Loaded private key ring %s", secretKeyRingPath)

	if publicKeyRingPath != "" {
		if err := pgp.LoadPublicRing(publicKeyRingPath); err != nil {
			log.AppLogger.Errorf("Could not load public keyring due to an error - %v", err)
			return errInvalidInput
		}
	}
	log.AppLogger.Infof("Loaded public key ring %s", publicKeyRingPath)

	if err := setupGlobalVars(); err != nil {
		return err
	}
	log.AppLogger.Infof("Setting working directory to %s", workingDirectory)
	pgp.PrintPGPDebugInformation()
	return nil
}

func getAndDecryptPrivateKey(email string) (*openpgp.Entity, error) {
	var entity *openpgp.Entity
	if entity = pgp.GetPrivateKeyByEmail(email); entity == nil {
		log.AppLogger.Errorf("Could not find private key for %s", email)
		return nil, errInvalidInput
	}

	if entity.PrivateKey != nil && entity.PrivateKey.Encrypted {
		validatePassphrase()
		if err := entity.PrivateKey.Decrypt(passphrase); err != nil {
			log.AppLogger.Errorf("Error decrypting private key: %v", err)
			return nil, errInvalidInput
		}
	}

	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			validatePassphrase()
			if err := subkey.PrivateKey.Decrypt(passphrase); err != nil {
				log.AppLogger.Errorf("Error decrypting subkey's private key: %v", err)
				return nil, errInvalidInput
			}
		}
	}

	return entity, nil
}

func loadSendKeys() error {
	if jobInfo.EncryptTo != "" {
		switch {
		case usingSmartOption() && secretKeyRingPath == "":
			log.AppLogger.Errorf("You must specify a secret keyring path if you use a smart option with encryptTo")
			return errInvalidInput
		case jobInfo.Resume && secretKeyRingPath == "":
			// The cached partial manifest is encrypted: without the secret ring a resume could
			// never read it, and would start over every time.
			log.AppLogger.Errorf("You must specify a secret keyring path if you use --resume with encryptTo")
			return errInvalidInput
		case publicKeyRingPath == "":
			log.AppLogger.Errorf("You must specify a public keyring path if you provide an encryptTo option")
			return errInvalidInput
		}
	}

	if jobInfo.SignFrom != "" && secretKeyRingPath == "" {
		log.AppLogger.Errorf("You must specify a secret keyring path if you provide a signFrom option")
		return errInvalidInput
	}

	if jobInfo.EncryptTo != "" {
		if usingSmartOption() || jobInfo.Resume {
			var err error
			jobInfo.EncryptKey, err = getAndDecryptPrivateKey(jobInfo.EncryptTo)
			if err != nil {
				return err
			}
		} else if jobInfo.EncryptKey = pgp.GetPublicKeyByEmail(jobInfo.EncryptTo); jobInfo.EncryptKey == nil {
			log.AppLogger.Errorf("Could not find public key for %s", jobInfo.EncryptTo)
			return errInvalidInput
		}
	}

	if jobInfo.SignFrom != "" {
		var err error
		jobInfo.SignKey, err = getAndDecryptPrivateKey(jobInfo.SignFrom)
		if err != nil {
			return err
		}
	}

	return loadTrustedSigners()
}

func loadReceiveKeys() error {
	if jobInfo.EncryptTo != "" && secretKeyRingPath == "" {
		log.AppLogger.Errorf("You must specify a secret keyring path if you provide an encryptTo option")
		return errInvalidInput
	}

	if jobInfo.SignFrom != "" && publicKeyRingPath == "" {
		log.AppLogger.Errorf("You must specify a public keyring path if you provide a signFrom option")
		return errInvalidInput
	}

	if jobInfo.EncryptTo != "" {
		var err error
		jobInfo.EncryptKey, err = getAndDecryptPrivateKey(jobInfo.EncryptTo)
		if err != nil {
			return err
		}
	}

	if jobInfo.SignFrom != "" {
		if jobInfo.SignKey = pgp.GetPublicKeyByEmail(jobInfo.SignFrom); jobInfo.SignKey == nil {
			log.AppLogger.Errorf("Could not find public key for %s", jobInfo.SignFrom)
			return errInvalidInput
		}
	}

	return loadTrustedSigners()
}

// loadTrustedSigners resolves each --trustSigner to the keys in the public ring it names. The
// resume cache still matches --signFrom's key alone.
func loadTrustedSigners() error {
	jobInfo.TrustedSignKeys = nil
	if len(trustSigners) == 0 {
		return nil
	}
	if jobInfo.SignFrom == "" {
		log.AppLogger.Errorf("--trustSigner adds to the keys --signFrom accepts; pass --signFrom as well")
		return errInvalidInput
	}
	if publicKeyRingPath == "" {
		log.AppLogger.Errorf("You must specify a public keyring path if you provide a trustSigner option")
		return errInvalidInput
	}
	for _, signer := range trustSigners {
		keys := pgp.GetPublicKeys(signer)
		if len(keys) == 0 {
			log.AppLogger.Errorf("Could not find public key for --trustSigner %s", signer)
			return errInvalidInput
		}
		jobInfo.TrustedSignKeys = append(jobInfo.TrustedSignKeys, keys...)
	}
	return nil
}

func postRunCleanup(cmd *cobra.Command, args []string) {
	err := os.RemoveAll(config.BackupTempdir)
	if err != nil {
		log.AppLogger.Errorf("Could not clean working temporary directory - %v", err)
	}
}

// nolint:gocyclo,funlen // Will do later
func setupGlobalVars() error {
	// Setup Tempdir

	if strings.HasPrefix(workingDirectory, "~") {
		usr, err := user.Current()
		if err != nil {
			log.AppLogger.Errorf("Could not get current user due to error - %v", err)
			return err
		}
		workingDirectory = filepath.Join(usr.HomeDir, strings.TrimPrefix(workingDirectory, "~"))
	}

	if dir, serr := os.Stat(workingDirectory); serr == nil && !dir.IsDir() {
		log.AppLogger.Errorf(
			"Cannot create working directory because another non-directory object already exists in that path (%s)",
			workingDirectory,
		)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(workingDirectory, 0755)
		if err != nil {
			log.AppLogger.Errorf("Could not create working directory %s due to error - %v", workingDirectory, err)
			return err
		}
	}

	dirPath := filepath.Join(workingDirectory, "temp")
	if dir, serr := os.Stat(dirPath); serr == nil && !dir.IsDir() {
		log.AppLogger.Errorf(
			"Cannot create temp dir in working directory because another non-directory object already exists in that path (%s)",
			dirPath,
		)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(dirPath, 0755)
		if err != nil {
			log.AppLogger.Errorf("Could not create temp directory %s due to error - %v", dirPath, err)
			return err
		}
	}

	tempdir, err := os.MkdirTemp(dirPath, config.ProgramName)
	if err != nil {
		log.AppLogger.Errorf("Could not create temp directory due to error - %v", err)
		return err
	}

	config.BackupTempdir = tempdir
	config.WorkingDir = workingDirectory

	dirPath = filepath.Join(workingDirectory, "cache")
	if dir, serr := os.Stat(dirPath); serr == nil && !dir.IsDir() {
		log.AppLogger.Errorf(
			"Cannot create cache dir in working directory because another non-directory object already exists in that path (%s)",
			dirPath,
		)
		return errInvalidInput
	} else if serr != nil {
		err := os.Mkdir(dirPath, 0755)
		if err != nil {
			log.AppLogger.Errorf("Could not create cache directory %s due to error - %v", dirPath, err)
			return err
		}
	}

	if maxUploadSpeed != 0 {
		log.AppLogger.Infof("Limiting the upload speed to %s/s.", humanize.Bytes(maxUploadSpeed*humanize.KByte))
		config.BackupUploadBucket = ratelimit.NewBucketWithRate(float64(maxUploadSpeed*humanize.KByte), int64(maxUploadSpeed*humanize.KByte))
	}
	return nil
}

func validatePassphrase() {
	var err error
	if len(passphrase) == 0 {
		fmt.Fprint(config.Stdout, "Enter passphrase to decrypt encryption key: ")
		passphrase, err = term.ReadPassword(0)
		if err != nil {
			log.AppLogger.Errorf("Error reading user input for encryption key passphrase: %v", err)
			panic(err)
		}
	}
}

// parseDestinations splits a comma-separated destination argument and canonicalizes each URI,
// so that e.g. s3://bucket/prefix and s3://bucket/prefix/ name the same destination and cache.
// typed maps each canonical URI back to the spelling given.
func parseDestinations(arg string) (destinations []string, typed map[string]string) {
	destinations = strings.Split(arg, ",")
	typed = make(map[string]string, len(destinations))
	for idx, raw := range destinations {
		destinations[idx] = backends.CanonicalURI(raw)
		typed[destinations[idx]] = raw
	}
	return destinations, typed
}
