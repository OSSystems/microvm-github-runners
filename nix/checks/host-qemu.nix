{
  pkgs,
  flake,
  system,
  ...
}:
pkgs.writeText "host-qemu-eval" (
  builtins.unsafeDiscardStringContext
    (flake.lib.hostConfig system "qemu" { poolSize = 2; }).system.build.toplevel.drvPath
)
