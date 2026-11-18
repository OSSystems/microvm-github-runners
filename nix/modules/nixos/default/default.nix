{ inputs, ... }:
{
  imports = [
    inputs.microvm.nixosModules.host
    inputs.vmkit.nixosModules.default
    ./host.nix
  ];
  services.microvm-github-runners = {
    microvmGuestModule = inputs.microvm.nixosModules.microvm;
    vmkitSource = inputs.vmkit;
  };
}
