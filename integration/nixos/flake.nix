# The NixOS system of the integration VM (integration/lima.yaml): nixos-lima's
# base module plus zfs and Docker. make integration-vm-up applies it with
#   nixos-rebuild boot --flake path:<copy>#zfsbackup-<arch>
# and restarts the VM, once. Keep nixpkgs on the branch the nixos-lima tag
# uses, so the rebuild changes only what zfs.nix asks for.
{
  inputs = {
    nixpkgs.url = "nixpkgs/nixos-26.05";
    nixos-lima = {
      url = "github:nixos-lima/nixos-lima/v0.2.2";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };
  outputs =
    { nixpkgs, nixos-lima, ... }:
    let
      system =
        arch:
        nixpkgs.lib.nixosSystem {
          system = "${arch}-linux";
          modules = [
            "${nixos-lima}/lima.nix"
            ./zfs.nix
          ];
        };
    in
    {
      nixosConfigurations.zfsbackup-aarch64 = system "aarch64";
      nixosConfigurations.zfsbackup-x86_64 = system "x86_64";
    };
}
