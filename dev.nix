# dev.nix — build/install zfsbackup-go from a specific git revision.
#
#   nix-build dev.nix --argstr rev <sha> --argstr hash <sri>   # build a revision -> ./result/bin/zfsbackup
#   nix-env -f dev.nix -i --argstr rev <sha> --argstr hash <sri>   # install it into your profile
#   nix-shell dev.nix                       # dev shell with go + the build deps
#
# Bumping the revision:
#   1. change `rev` (and `version` if you like)
#   2. set `hash` to lib.fakeHash (or just delete the string), rebuild, and
#      copy the "got:" hash from the error into `hash`.
#
# Deps are vendored in-tree, so vendorHash is null and no network fetch of
# modules happens during the build.

{
  pkgs ? import <nixpkgs> { },

  # Git revision to build. There is no usable default: `rev` and `hash` must
  # both be passed (--argstr rev <sha> --argstr hash <sri>), or pinned here.
  # TODO: pin `rev` to a release and `hash` to its real value (step 2 below
  # gets it; nix is not on the development host, so this is still open).
  rev ? "master",

  # Source hash for that revision. Update whenever `rev` changes; with the
  # placeholder the build fails on purpose and prints the hash to use.
  hash ? pkgs.lib.fakeHash,

  owner ? "bosgood",
  repo ? "zfsbackup-go",

  version ? builtins.substring 0 12 rev,

  # go.mod requires go >= 1.25, and the build sandbox has no network, so
  # GOTOOLCHAIN can't fetch one — the toolchain must come from nixpkgs.
  # Falls back to the default `go` on channels new enough not to need it.
  go ? pkgs.go_1_25 or pkgs.go,
}:

(pkgs.buildGoModule.override { inherit go; }) {
  pname = "zfsbackup-go";
  inherit version;

  src = pkgs.fetchFromGitHub { inherit owner repo rev hash; };

  # ./vendor is checked in.
  vendorHash = null;

  # The upstream module path, regardless of which fork we build from.
  ldflags = [
    "-w"
    "-s"
    "-X github.com/someone1/zfsbackup-go/config.GitCommit=${rev}"
  ];

  # Integration tests need a real ZFS pool and root.
  checkFlags = [ "-short" ];

  meta = with pkgs.lib; {
    description = "Backup ZFS snapshots to cloud storage";
    homepage = "https://github.com/${owner}/${repo}";
    license = licenses.mit;
    mainProgram = "zfsbackup-go";
    platforms = platforms.unix;
  };
}
