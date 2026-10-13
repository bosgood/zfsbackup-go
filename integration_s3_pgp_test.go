//go:build integration

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/someone1/zfsbackup-go/cmd"
	"github.com/someone1/zfsbackup-go/files"
)

// pgpFixture is one keyring pair in testdata/pgp, as GnuPG exported it. Its
// key's email is <prefix>@example.com.
type pgpFixture struct {
	prefix, passphrase string
}

// itPGPFixtures are the testdata/pgp fixtures (make pgp-fixtures).
var itPGPFixtures = []pgpFixture{
	{"ed25519", ""},
	{"ed25519-pass", "zfsbackup-test"},
	{"rsa", ""},
}

// pgpSendArgs are the send flags of a single full send in these tests. No
// compressor: a decrypted volume is the zfs stream itself.
var pgpSendArgs = []string{"--volsize", "1", "--compressor", ""}

// ringFlags returns the ring path flags of one fixture, relative to the repo
// root: the test runs from there (make integration) or from /src (the image).
func ringFlags(prefix string) []string {
	return []string{
		"--publicKeyRingPath", "testdata/pgp/" + prefix + "-public.asc",
		"--secretKeyRingPath", "testdata/pgp/" + prefix + "-secret.asc",
	}
}

// fixtureRings returns the key flags for one fixture: its rings, and its key
// as --encryptTo and --signFrom.
func fixtureRings(prefix string) []string {
	email := prefix + "@example.com"
	return append(ringFlags(prefix), "--encryptTo", email, "--signFrom", email)
}

// fixtureKey reads the secret ring of one fixture and returns its one key.
func fixtureKey(t *testing.T, prefix string) *openpgp.Entity {
	t.Helper()
	f, err := os.Open("testdata/pgp/" + prefix + "-secret.asc")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	keys, err := openpgp.ReadArmoredKeyRing(f)
	if err != nil {
		t.Fatalf("fixture %s: %v", prefix, err)
	}
	if len(keys) != 1 {
		t.Fatalf("fixture %s holds %d keys, want 1", prefix, len(keys))
	}
	return keys[0]
}

// fingerprint is the fingerprint of e's primary key as a manifest records it.
func fingerprint(e *openpgp.Entity) string {
	return fmt.Sprintf("%X", e.PrimaryKey.Fingerprint)
}

// combinedRings writes one public and one secret ring that hold the key of
// every fixture in prefixes, and returns their flags with email as
// --encryptTo and --signFrom.
func combinedRings(t *testing.T, email string, prefixes ...string) []string {
	t.Helper()
	keys := make([]*openpgp.Entity, 0, len(prefixes))
	for _, p := range prefixes {
		keys = append(keys, fixtureKey(t, p))
	}
	return writeRingsFor(t, email, keys...)
}

// wantSignedVolume fails unless data starts with a one-pass signature: the
// first packet openpgp.Sign writes. The counterpart of wantPGPVolume.
func wantSignedVolume(t *testing.T, name string, data []byte) {
	t.Helper()
	p, err := packet.NewReader(bytes.NewReader(data)).Next()
	if err != nil {
		t.Fatalf("%s: no OpenPGP packet: %v", name, err)
	}
	if _, ok := p.(*packet.OnePassSignature); !ok {
		t.Fatalf("%s starts with a %T, want a *packet.OnePassSignature", name, p)
	}
}

// wantPGPObjects fails unless every object under prefix has the pgp extension
// (encrypted and signed-only objects both have it), and want accepts the
// bytes of each manifest and of each first volume.
func wantPGPObjects(t *testing.T, client *s3.S3, bucket, prefix string, want func(*testing.T, string, []byte)) {
	t.Helper()
	names := objectNames(t, client, bucket, prefix)
	checked := 0
	for _, n := range names {
		if !strings.Contains(n, ".pgp") {
			t.Errorf("object %s has no .pgp extension; all objects: %v", n, names)
		}
		if strings.Contains(n, ".manifest") || strings.HasSuffix(n, ".vol1") {
			want(t, n, objectBytes(t, client, bucket, prefix+n))
			checked++
		}
	}
	if checked < 2 {
		t.Fatalf("want a manifest and a first volume at %s, there are %v", prefix, names)
	}
}

// newPGPSource makes <pool>/<name> with a 3 MiB file and one snapshot, and
// returns the snapshot's name. It destroys the dataset when the test ends.
func newPGPSource(t *testing.T, src string) string {
	t.Helper()
	destroyAfter(t, src)
	zfsRun(t, "create", "-o", "mountpoint=/"+src, src)
	writeFile(t, "/"+src, "a.bin", 3<<20)
	return snapshot(t, src, "daily")
}

// pgpSend runs one full send of src@snap to target with keys, and fails the test on an error.
func pgpSend(t *testing.T, work, src, snap, target string, keys ...string) {
	t.Helper()
	args := append(append(slices.Clone(keys), pgpSendArgs...), src+"@"+snap, target)
	if logs, err := itSend(t, work, args...); err != nil {
		t.Fatalf("send %s: %v\n%s", strings.Join(args, " "), err, logs)
	}
}

// pgpRestore receives src@snap from target into restored with keys, from an
// empty working directory (the manifest comes from the destination), and
// checks the restored dataset against src.
func pgpRestore(t *testing.T, src, snap, target, restored string, keys ...string) {
	t.Helper()
	itReceive(t, t.TempDir(), append(slices.Clone(keys), "-F", src+"@"+snap, target, restored)...)
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)
}

// TestIntegrationS3PGPKeys takes each GnuPG-exported fixture (Ed25519, Ed25519
// with a passphrase, RSA) through send, list and receive on real zfs and S3,
// and checks that another fixture's key and a wrong passphrase fail.
func TestIntegrationS3PGPKeys(t *testing.T) {
	pool := testPool(t)
	for i, f := range itPGPFixtures {
		wrong := itPGPFixtures[(i+1)%len(itPGPFixtures)]
		t.Run(f.prefix, func(t *testing.T) { testPGPKey(t, pool, f, wrong) })
	}
}

func testPGPKey(t *testing.T, pool string, f, wrong pgpFixture) {
	t.Setenv("PGP_PASSPHRASE", f.passphrase)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-pgp-"+f.prefix)
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcpgp" + f.prefix
	restored := pool + "/restoredPGP" + f.prefix
	restoredWrong := restored + "w"
	destroyAfter(t, restored, restoredWrong)
	work := t.TempDir()
	keys := fixtureRings(f.prefix)
	snap := newPGPSource(t, src)
	pgpSend(t, work, src, snap, target, keys...)
	wantPGPObjects(t, client, bucket, prefix, wantPGPVolume)

	backups := itList(t, t.TempDir(), append(slices.Clone(keys), target)...)[src]
	if len(backups) != 1 || backups[0].BaseSnapshot.Name != snap {
		t.Fatalf("list of %s: got %v, want the one backup of %s", target, chainNames(backups), snap)
	}
	fpr := fingerprint(fixtureKey(t, f.prefix))
	if m := backups[0]; m.EncryptKeyFingerprint != fpr || m.SignKeyFingerprint != fpr {
		t.Errorf("the manifest records encrypt key %s and sign key %s, want %s for both",
			m.EncryptKeyFingerprint, m.SignKeyFingerprint, fpr)
	}
	pgpRestore(t, src, snap, target, restored, keys...)

	// Another fixture's key: the manifest does not open. The advice is to pass
	// the right ring, never to delete the manifest.
	t.Setenv("PGP_PASSPHRASE", wrong.passphrase)
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(),
		append(fixtureRings(wrong.prefix), "-F", src+"@"+snap, target, restoredWrong)...)
	if err == nil {
		t.Fatalf("receive with the %s key succeeded, want an error:\n%s", wrong.prefix, logs)
	}
	wantNoDataset(t, restoredWrong)
	if !strings.Contains(logs, "--secretKeyRingPath") || strings.Contains(logs, "delete that object") {
		t.Errorf("receive with the %s key: want advice to pass --secretKeyRingPath and none to delete:\n%s", wrong.prefix, logs)
	}

	if f.passphrase != "" {
		testWrongPassphrase(t, src, f.prefix)
	}
}

// testWrongPassphrase sends with a wrong PGP_PASSPHRASE: the send fails while
// it loads the keys, before it writes anything to the destination.
func testWrongPassphrase(t *testing.T, src, prefix string) {
	t.Helper()
	client, bucket, s3prefix := itBucket(t, "zfsbackup-it-pgp-wrongpass")
	target := "s3://" + bucket + "/" + s3prefix
	t.Setenv("PGP_PASSPHRASE", "wrong")
	snap := snapshot(t, src, "hourly")
	logs, err := itSend(t, t.TempDir(), append(append(fixtureRings(prefix), pgpSendArgs...), src+"@"+snap, target)...)
	if err == nil {
		t.Fatalf("send with a wrong passphrase succeeded, want an error:\n%s", logs)
	}
	if !strings.Contains(logs, "Error decrypting private key") {
		t.Errorf("send with a wrong passphrase: want %q in the log:\n%s", "Error decrypting private key", logs)
	}
	if volumes, manifests, list := countObjects(t, client, bucket, s3prefix); volumes+manifests != 0 {
		t.Errorf("send with a wrong passphrase left objects at %s:\n%s", target, list)
	}
}

// TestIntegrationS3PGPModes sends one snapshot encrypted only and signed only,
// restores both, receives the encrypted one in pipe mode, and checks that a
// tampered signed volume fails the restore.
func TestIntegrationS3PGPModes(t *testing.T) {
	const fix = "ed25519"
	email := fix + "@example.com"
	pool := testPool(t)
	client, bucket, prefix := itBucket(t, "zfsbackup-it-pgpmodes")
	encPrefix, signPrefix := prefix+"enc/", prefix+"sign/"
	encTarget := "s3://" + bucket + "/" + encPrefix
	signTarget := "s3://" + bucket + "/" + signPrefix
	src := pool + "/srcpgpmodes"
	restoredEnc := pool + "/restoredPGPEnc"
	restoredSign := pool + "/restoredPGPSign"
	restoredPipe := pool + "/restoredPGPPipe"
	restoredBad := pool + "/restoredPGPBad"
	destroyAfter(t, restoredEnc, restoredSign, restoredPipe, restoredBad)
	snap := newPGPSource(t, src)
	encrypt := append(ringFlags(fix), "--encryptTo", email)
	sign := append(ringFlags(fix), "--signFrom", email)

	pgpSend(t, t.TempDir(), src, snap, encTarget, encrypt...)
	wantPGPObjects(t, client, bucket, encPrefix, wantPGPVolume)
	pgpRestore(t, src, snap, encTarget, restoredEnc, encrypt...)
	// Pipe mode streams each volume into zfs receive as it downloads: encryption
	// is allowed there, signing is not (TestReceiveRefusesStreamingWithSignFrom).
	pgpRestore(t, src, snap, encTarget, restoredPipe, append(slices.Clone(encrypt), "--maxFileBuffer", "0")...)

	pgpSend(t, t.TempDir(), src, snap, signTarget, sign...)
	wantPGPObjects(t, client, bucket, signPrefix, wantSignedVolume)
	pgpRestore(t, src, snap, signTarget, restoredSign, append(slices.Clone(sign), "--maxFileBuffer", "5")...)

	// One flipped byte in the middle of volume 2: it no longer matches the
	// SHA-256 the signed manifest records, so zfs receive never reads it. The
	// log names the volume and its manifest, not a missing key.
	vol2 := manifestFor(t, t.TempDir(), src, signTarget, snap, sign...).Volumes[1].ObjectName
	data := objectBytes(t, client, bucket, signPrefix+vol2)
	data[len(data)/2] ^= 0xff
	putObject(t, client, bucket, signPrefix+vol2, data)
	logs, err := itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(),
		append(slices.Clone(sign), "--maxFileBuffer", "5", "-F", src+"@"+snap, signTarget, restoredBad)...)
	if err == nil {
		t.Fatalf("receive of a tampered %s succeeded, want an error:\n%s", vol2, logs)
	}
	wantNoDataset(t, restoredBad)
	if want := vol2 + " does not match its manifest"; !strings.Contains(logs, want) || strings.Contains(logs, "--secretKeyRingPath") {
		t.Errorf("receive of a tampered %s: want %q in the log and no key-ring advice:\n%s", vol2, want, logs)
	}
}

// TestIntegrationS3PGPRotation rotates the signing key from RSA to Ed25519
// on a chain at S3: smart sends and receive --auto refuse the old signatures
// unless --trustSigner names the RSA key. TestE2ESignKeyRotationWithTrustSigner
// across algorithms, on real zfs.
func TestIntegrationS3PGPRotation(t *testing.T) {
	const oldFix, newFix = "rsa", "ed25519"
	pool := testPool(t)
	_, bucket, prefix := itBucket(t, "zfsbackup-it-pgprot")
	target := "s3://" + bucket + "/" + prefix
	restored := pool + "/restoredPGPRot"
	restoredNoTrust := pool + "/restoredPGPRotNT"
	destroyAfter(t, restored, restoredNoTrust)
	work := t.TempDir()
	oldKeys := fixtureRings(oldFix)
	c := buildChain(t, pool, work, target, "pgprot", oldKeys...)
	oldFpr, newFpr := fingerprint(fixtureKey(t, oldFix)), fingerprint(fixtureKey(t, newFix))
	trust := []string{"--trustSigner", oldFpr}

	// Both keys in both rings; the new key encrypts and signs.
	rings := combinedRings(t, newFix+"@example.com", oldFix, newFix)
	smart := append(slices.Clone(c.smart[:len(c.smart)-len(oldKeys)]), rings...)
	writeFile(t, "/"+c.src, "d.bin", 1<<20)
	daily4 := snapshot(t, c.src, "daily")

	logs, err := itSend(t, work, append(slices.Clone(smart), c.src, target)...)
	if err == nil {
		t.Fatalf("a smart send accepted manifests signed by the old key without --trustSigner:\n%s", logs)
	}
	if want := "signed by " + oldFpr + ", want " + newFpr; !strings.Contains(logs, want) || !strings.Contains(logs, "--trustSigner") {
		t.Errorf("the send failed, but its log does not say %q and point at --trustSigner:\n%s", want, logs)
	}
	listed := func() []string {
		t.Helper()
		return chainNames(itList(t, t.TempDir(), append(slices.Clone(rings), append(trust, target)...)...)[c.src])
	}
	if got := listed(); len(got) != 3 {
		t.Fatalf("after the refused send %s lists %v, want the 3 links of before", target, got)
	}

	if logs, err = itSend(t, work, append(append(slices.Clone(smart), trust...), c.src, target)...); err != nil {
		t.Fatalf("smart send with --trustSigner %s: %v\n%s", oldFpr, err, logs)
	}
	if got, want := listed(), daily4+"<-"+c.daily3; len(got) != 4 || got[3] != want {
		t.Fatalf("after the rotation %s lists %v, want a 4th link %s", target, got, want)
	}
	if m := manifestFor(t, t.TempDir(), c.src, target, daily4, append(slices.Clone(rings), trust...)...); m.SignKeyFingerprint != newFpr {
		t.Errorf("the new manifest is signed by %s, want the new key %s", m.SignKeyFingerprint, newFpr)
	}

	receive := append(slices.Clone(rings), "--auto", "-F", c.src, target)
	itReceive(t, t.TempDir(), append(append(slices.Clone(trust), receive...), restored)...)
	wantSnapshots(t, restored, c.monthly, c.daily2, c.daily3, daily4)
	wantGUIDs(t, c.src, restored, c.monthly, c.daily2, c.daily3, daily4)
	compareDirs(t, "/"+c.src, "/"+restored)

	if logs, err = itRun(cmd.ResetReceiveJobInfo, "receive", t.TempDir(), append(receive, restoredNoTrust)...); err == nil {
		t.Fatalf("receive of a chain spanning the rotation succeeded without --trustSigner:\n%s", logs)
	}
	wantNoDataset(t, restoredNoTrust)
	if want := "signed by " + oldFpr + ", want " + newFpr; !strings.Contains(err.Error()+logs, want) ||
		!strings.Contains(err.Error()+logs, "--trustSigner") {
		t.Errorf("the receive failed, but not because of the old signer: want %q and --trustSigner: %v\n%s", want, err, logs)
	}
}

// gpgRunner returns a runner for gpg commands against a throwaway GNUPGHOME
// that unlock keys with passphrase; it skips the test without gpg. Like
// gpgHome in files/volumeinfo_gpg_test.go.
func gpgRunner(t *testing.T, passphrase string) func(args ...string) ([]byte, error) {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	// Not t.TempDir(): gpg-agent puts its socket in the home directory and
	// that path is too long for a Unix socket on macOS.
	home, err := os.MkdirTemp("", "gpg")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) ([]byte, error) {
		c := exec.Command("gpg", append([]string{"--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", passphrase}, args...)...)
		c.Env = append(os.Environ(), "GNUPGHOME="+home)
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr = &out, &errb
		if runErr := c.Run(); runErr != nil {
			return out.Bytes(), fmt.Errorf("gpg %s: %w: %s", strings.Join(args, " "), runErr, errb.String())
		}
		return out.Bytes(), nil
	}
	t.Cleanup(func() {
		kill := exec.Command("gpgconf", "--kill", "gpg-agent")
		kill.Env = append(os.Environ(), "GNUPGHOME="+home)
		_ = kill.Run()
		os.RemoveAll(home)
	})
	return run
}

// gpgRunbookKey makes a key with the gpg commands of step 0 of
// docs/runbook-first-backup.md (Ed25519 and a cv25519 subkey, protected with
// the runner's passphrase), exports its rings, and returns them as key flags.
// The testdata/pgp fixtures are from gpg 2.2; this key is from the gpg of the
// image, the version an operator runs.
func gpgRunbookKey(t *testing.T, run func(args ...string) ([]byte, error), email string) []string {
	t.Helper()
	version, err := run("--version")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", bytes.SplitN(version, []byte("\n"), 2)[0])
	if _, err = run("--quick-gen-key", "zfsbackup <"+email+">", "ed25519", "sign,cert", "never"); err != nil {
		t.Fatal(err)
	}
	colons, err := run("--list-keys", "--with-colons", email)
	if err != nil {
		t.Fatal(err)
	}
	var fpr string
	for _, line := range strings.Split(string(colons), "\n") {
		if f := strings.Split(line, ":"); len(f) > 9 && f[0] == "fpr" {
			fpr = f[9]
			break
		}
	}
	if fpr == "" {
		t.Fatalf("gpg lists no fingerprint for %s:\n%s", email, colons)
	}
	if _, err = run("--quick-add-key", fpr, "cv25519", "encr", "never"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pub, sec := filepath.Join(dir, "pubring.asc"), filepath.Join(dir, "secring.asc")
	if _, err = run("--armor", "--output", pub, "--export", email); err != nil {
		t.Fatal(err)
	}
	if _, err = run("--armor", "--output", sec, "--export-secret-keys", email); err != nil {
		t.Fatal(err)
	}
	return []string{"--publicKeyRingPath", pub, "--secretKeyRingPath", sec, "--encryptTo", email, "--signFrom", email}
}

// TestIntegrationS3PGPInterop has stock gpg decrypt and verify every object a
// send uploaded, and feeds the decrypted volumes to zfs receive: the objects
// are plain OpenPGP messages, and a restore needs no zfsbackup. Subtest
// runbook makes its key with the runbook's gpg commands.
func TestIntegrationS3PGPInterop(t *testing.T) {
	pool := testPool(t)
	for _, prefix := range []string{"ed25519", "rsa"} {
		t.Run(prefix, func(t *testing.T) {
			run := gpgRunner(t, "")
			if _, err := run("--import", "testdata/pgp/"+prefix+"-secret.asc"); err != nil {
				t.Fatal(err)
			}
			testPGPInterop(t, pool, prefix, run, fixtureRings(prefix))
		})
	}
	t.Run("runbook", func(t *testing.T) {
		const passphrase = "zfsbackup-runbook"
		run := gpgRunner(t, passphrase)
		keys := gpgRunbookKey(t, run, "runbook@example.com")
		t.Setenv("PGP_PASSPHRASE", passphrase)
		testPGPInterop(t, pool, "runbook", run, keys)
	})
}

// testPGPInterop sends a new dataset <pool>/srcpgpgpg<name> with keys, restores
// it with zfsbackup, and then with run (a gpg that holds the secret key) and
// zfs receive alone.
func testPGPInterop(t *testing.T, pool, name string, run func(args ...string) ([]byte, error), keys []string) {
	client, bucket, prefix := itBucket(t, "zfsbackup-it-pgpgpg-"+name)
	target := "s3://" + bucket + "/" + prefix
	src := pool + "/srcpgpgpg" + name
	restored, restoredZB := pool+"/restoredGPG"+name, pool+"/restoredPGPI"+name
	destroyAfter(t, restored, restoredZB)
	snap := newPGPSource(t, src)
	pgpSend(t, t.TempDir(), src, snap, target, keys...)
	pgpRestore(t, src, snap, target, restoredZB, keys...)

	dir := t.TempDir()
	decrypt := func(object string) []byte {
		t.Helper()
		in, out, status := filepath.Join(dir, "in"), filepath.Join(dir, "out"), filepath.Join(dir, "status")
		for _, p := range []string{in, out, status} {
			_ = os.Remove(p)
		}
		if err := os.WriteFile(in, objectBytes(t, client, bucket, prefix+object), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := run("--trust-model", "always", "--status-file", status, "--output", out, "--decrypt", in); err != nil {
			t.Fatalf("%s: %v", object, err)
		}
		// gpg checks the signature while it decrypts (--verify refuses an
		// encrypted message); the status lines say whether it was good.
		st, err := os.ReadFile(status)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"[GNUPG:] DECRYPTION_OKAY", "[GNUPG:] GOODSIG ", "[GNUPG:] VALIDSIG "} {
			if !bytes.Contains(st, []byte(want)) {
				t.Errorf("%s: gpg status has no %q:\n%s", object, want, st)
			}
		}
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}

	// A manifest is always gzipped (--compressor is for volumes).
	var manifest files.JobInfo
	var manifests []string
	for _, n := range objectNames(t, client, bucket, prefix) {
		if !strings.Contains(n, ".manifest") {
			continue
		}
		if manifests = append(manifests, n); len(manifests) > 1 {
			t.Fatalf("want one manifest at %s, there are %v", target, manifests)
		}
		zr, err := gzip.NewReader(bytes.NewReader(decrypt(n)))
		if err != nil {
			t.Fatalf("the manifest gpg decrypted is not gzip: %v", err)
		}
		if err = json.NewDecoder(zr).Decode(&manifest); err != nil {
			t.Fatalf("the manifest gpg decrypted is not JSON: %v", err)
		}
	}
	if len(manifests) == 0 || len(manifest.Volumes) == 0 {
		t.Fatalf("want a manifest with volumes at %s, there are %v", target, manifests)
	}
	if manifest.VolumeName != src || manifest.BaseSnapshot.Name != snap {
		t.Errorf("the manifest gpg decrypted is of %s@%s, want %s@%s", manifest.VolumeName, manifest.BaseSnapshot.Name, src, snap)
	}

	// The volume names and order come from the manifest gpg decrypted, not from zfsbackup.
	slices.SortFunc(manifest.Volumes, func(a, b *files.VolumeInfo) int { return int(a.VolumeNumber - b.VolumeNumber) })
	var stream bytes.Buffer
	for _, v := range manifest.Volumes {
		stream.Write(decrypt(v.ObjectName))
	}
	recv := exec.Command("zfs", "receive", "-F", restored)
	recv.Stdin = &stream
	if out, err := recv.CombinedOutput(); err != nil {
		t.Fatalf("zfs receive of the volumes gpg decrypted: %v: %s", err, out)
	}
	wantSnapshots(t, restored, snap)
	wantGUIDs(t, src, restored, snap)
	compareDirs(t, "/"+src, "/"+restored)
}
