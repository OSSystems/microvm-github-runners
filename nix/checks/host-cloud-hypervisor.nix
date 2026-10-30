{
  pkgs,
  flake,
  system,
  ...
}:
pkgs.writeText "host-cloud-hypervisor-eval" (
  builtins.unsafeDiscardStringContext
    (flake.lib.hostConfig system "cloud-hypervisor" { }).system.build.toplevel.drvPath
)
