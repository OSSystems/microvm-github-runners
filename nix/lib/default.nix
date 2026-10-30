{ flake, inputs, ... }:
{
  hostConfig =
    system: backend: extra:
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
      ];
    }).config;
}
