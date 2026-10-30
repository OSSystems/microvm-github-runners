{
  pkgs,
  flake,
  system,
  ...
}:
let
  execStart =
    backend: extra:
    (flake.lib.hostConfig system backend extra)
    .systemd.services.microvm-scaleset.serviceConfig.ExecStart;
in
pkgs.runCommand "controller-flags" { } ''
  ${execStart "cloud-hypervisor" { }} --help
  ${execStart "qemu" { poolSize = 2; }} --help
  touch $out
''
