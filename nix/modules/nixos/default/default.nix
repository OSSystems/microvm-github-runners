{ inputs, ... }:
{
  imports = [
    inputs.microvm.nixosModules.host
    ./host.nix
  ];
  services.microvm-github-runners.microvmGuestModule = inputs.microvm.nixosModules.microvm;
}
