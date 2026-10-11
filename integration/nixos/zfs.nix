# zfs and Docker for the integration VM. The test container runs this
# system's zfs userland (/nix/store is mounted into it, see
# integration/compose.nixos.yaml), so userland and module are one build.
{ pkgs, lib, ... }:
{
  boot.supportedFilesystems = [ "zfs" ];
  boot.kernelModules = [ "zfs" ];
  # nixos-lima runs linuxPackages_latest, which the zfs module often does not
  # support yet. The LTS kernel always is.
  boot.kernelPackages = lib.mkForce pkgs.linuxPackages;
  # No pool of the VM's own: nothing to import at boot.
  boot.zfs.forceImportRoot = false;
  # zfs refuses to load without a host id.
  networking.hostId = "7a6b5c4d";

  virtualisation.docker.enable = true;
  # lima-init creates the Lima user at boot and puts it in wheel, which has
  # sudo without a password already. Its group list is fixed per ssh session
  # (Lima reuses one connection), so a docker group added later would not
  # apply; the socket goes to wheel instead.
  systemd.sockets.docker.socketConfig.SocketGroup = lib.mkForce "wheel";

  # make runs integration-docker inside the VM.
  environment.systemPackages = [ pkgs.gnumake ];
}
