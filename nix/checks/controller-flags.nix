{
  pkgs,
  flake,
  system,
  ...
}:
let
  execStartWith =
    backend: extra: module:
    (flake.lib.hostConfigWith system backend extra module)
    .systemd.services.microvm-scaleset.serviceConfig.ExecStart;
  execStart = backend: extra: execStartWith backend extra { };
  # With a pool ahead of them on the host, runners publish their memory and
  # read that pool's.
  behind = execStartWith "cloud-hypervisor" { guaranteedMemGiB = 4; } {
    services.vmkit.memory.pools.ahead.rank = "first";
  };
in
assert pkgs.lib.hasInfix "--ahead-status=/run/vmkit/memory/ahead.json" behind;
assert pkgs.lib.hasInfix "--memory-status=/run/vmkit/memory/microvm-github-runners.json" behind;
assert !(pkgs.lib.hasInfix "--ahead-status" (execStart "cloud-hypervisor" { }));
pkgs.runCommand "controller-flags" { } ''
  ${execStart "cloud-hypervisor" { }} --help
  ${execStart "cloud-hypervisor" { nestedVirtualization = true; }} --help
  ${behind} --help
  ${execStart "qemu" { poolSize = 2; }} --help
  touch $out
''
