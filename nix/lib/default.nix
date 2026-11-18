{ flake, inputs, ... }:
rec {
  hostConfig =
    system: backend: extra:
    hostConfigWith system backend extra { };

  # hostConfigWith also takes a NixOS module, for what lives outside
  # services.microvm-github-runners.
  hostConfigWith =
    system: backend: extra: module:
    (inputs.nixpkgs.lib.nixosSystem {
      inherit system;
      modules = [
        flake.nixosModules.default
        {
          boot.loader.grub.enable = false;
          fileSystems."/" = {
            device = "/dev/vda";
            fsType = "ext4";
          };
          system.stateVersion = "26.05";
          networking = {
            hostName = "ci-host";
            useDHCP = false;
          };
          services.microvm-github-runners = {
            enable = true;
            inherit backend;
            urls = [ "https://github.com/my-org" ];
            githubApp = {
              id = "1";
              privateKeyFile = "/run/keys/github-app.pem";
            };
            network.externalInterface = "eth0";
          }
          // extra;
        }
        module
      ];
    }).config;
}
