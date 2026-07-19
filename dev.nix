# dev.nix — build/install zfsbackup-go from a specific git revision.
#
#   nix-build dev.nix                       # build the pinned rev -> ./result/bin/zfsbackup
#   nix-env -f dev.nix -i                   # install it into your profile
#   nix-build dev.nix --argstr rev <sha>    # build some other revision
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

  # Git revision to build. Defaults to the fork's clean-dry-run tip.
  rev ? "master",

  # Source hash for that revision. Update whenever `rev` changes.
  hash ? pkgs.lib.fakeHash,

  owner ? "bosgood",
  repo ? "zfsbackup-go",

  version ? builtins.substring 0 12 rev,
}:

pkgs.buildGoModule {
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
